package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/workerssh"
)

func firstWorkerSleeper(args []string) int {
	if len(args) != 1 {
		return 2
	}
	// An acknowledgment proves the child installed its TERM handler before
	// cleanup. Escalation and its exit barrier must both be exercised.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
	if err := os.WriteFile(args[0], []byte("ready"), 0600); err != nil {
		return 2
	}
	for {
		time.Sleep(time.Second)
	}
}
func firstWorkerSentinel(t *testing.T, o *firstWorkerProcesses) *firstWorkerProcess {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	ready := filepath.Join(t.TempDir(), "ready")
	r, err := o.start(os.Args[0], []string{"first-worker-sleeper", ready}, os.Environ(), []*os.File{null, null, null})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.close(); err != nil {
			t.Error(err)
		}
		// Independent backstop keeps mutation runs from abandoning new children
		// when the close barrier itself is deliberately removed.
		if err := firstWorkerWaitExit([]*firstWorkerProcess{r}, nil); err != nil {
			t.Error(err)
		}
	})
	eventually(t, "fixture sentinel armed", 5*time.Second, func() bool { _, err := os.Stat(ready); return err == nil })
	return r
}
func TestFirstWorkerProcessOwnershipAndExitBarrier(t *testing.T) {
	owner, foreign := newFirstWorkerProcesses(), newFirstWorkerProcesses()
	own, other := firstWorkerSentinel(t, owner), firstWorkerSentinel(t, foreign)
	if err := owner.stop(other); !errors.Is(err, errFirstWorkerForeignProcess) {
		t.Errorf("foreign owner accepted: %v", err)
	}
	forged := &firstWorkerProcess{owner: owner, process: other.process, stop: make(chan struct{}), done: make(chan struct{})}
	if err := owner.stop(forged); !errors.Is(err, errFirstWorkerForeignProcess) {
		t.Errorf("PID-only record accepted: %v", err)
	}
	select {
	case <-other.stop:
		t.Error("foreign fixture was stopped")
	default:
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-own.done:
	default:
		t.Error("cleanup returned before owned child exit")
	}
	if own.signals.Load() < 2 {
		t.Error("TERM-resistant child did not exercise escalation")
	}
	signals := own.signals.Load()
	if err := owner.stop(own); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if own.signals.Load() != signals {
		t.Error("reaped process was signalled again")
	}
	select {
	case <-other.done:
		t.Error("foreign fixture lost its live child")
	default:
	}
}

func TestFirstWorkerSSHCleanupAcrossMainRestart(t *testing.T) {
	f := newFirstWorker(t)
	c := f.client()
	defer c.Close()
	id, conv := f.start(c, "cleanup", "reply before cleanup restart", "build")
	f.main.posted(t, conv, "before cleanup restart")
	f.ready(c, id)
	records := f.ssh.processes.snapshot()
	children, err := firstWorkerDescendants(records)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, r := range records {
		select {
		case <-r.done:
		default:
			live++
		}
	}
	if live < 2 || len(children) == 0 {
		t.Fatal("gate did not establish owned SSH master and server descendant")
	}
	f.restart()
	if err := firstWorkerWaitExit(records, children); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = f.client()
	f.ready(c, id)
	if err := c.Send(context.Background(), id, "cleanup-restart", control.Input{Text: "reply after cleanup restart"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	f.main.posted(t, conv, "after cleanup restart")
	// A normal explicit teardown with an established connection also proves the
	// exit barrier, before the test's redundant cleanup runs.
	f.main.stopDaemon()
	if err := f.ssh.close(); err != nil {
		t.Fatal(err)
	}
}

const firstWorkerFailureEnv = "HATAORI_TEST_FIRST_WORKER_FAILURE"

func TestFirstWorkerFixtureFailureCleanup(t *testing.T) {
	if os.Getenv(firstWorkerFailureEnv) == "1" {
		var f *firstWorker
		var records []*firstWorkerProcess
		var children []firstWorkerIdentity
		// Registered first: this observes cleanup after the deliberately failing
		// test has executed every fixture cleanup, including process waits.
		t.Cleanup(func() {
			if f == nil {
				return
			}
			if err := f.ssh.close(); err != nil {
				t.Error(err)
				return
			}
			if err := firstWorkerWaitExit(records, children); err != nil {
				t.Error(err)
				return
			}
			for _, r := range f.ssh.processes.snapshot() {
				select {
				case <-r.done:
				default:
					t.Error("failed fixture left owned child live")
					return
				}
			}
			t.Log("FAILED_FIXTURE_CHILDREN_CONFIRMED_EXITED")
		})
		f = newFirstWorker(t)
		records = f.ssh.processes.snapshot()
		var err error
		children, err = firstWorkerDescendants(records)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) == 0 {
			t.Fatal("failed fixture had no established SSH child")
		}
		t.Fatal("deliberate isolated fixture failure")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFirstWorkerFixtureFailureCleanup$", "-test.parallel=1", "-test.timeout=40s", "-test.v")
	cmd.Env = append(os.Environ(), firstWorkerFailureEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "deliberate isolated fixture failure") || !strings.Contains(string(output), "FAILED_FIXTURE_CHILDREN_CONFIRMED_EXITED") {
		t.Fatalf("failed fixture cleanup was not proved: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "still running") || strings.Contains(string(output), "not confirmed") {
		t.Fatalf("failed fixture cleanup error:\n%s", output)
	}
}

func TestFirstWorkerSSHAuthorityRejectsForeignConnection(t *testing.T) {
	f := newFirstWorker(t)
	o := f.ssh
	o.mu.Lock()
	var control string
	for path := range o.masters {
		control = path
		break
	}
	o.mu.Unlock()
	if control == "" {
		t.Fatal("no established owned master")
	}
	args := []string{"-F", "/dev/null", "-S", control, "-o", "ControlMaster=no", "-o", "ClearAllForwardings=no", "-o", "ExitOnForwardFailure=yes", "-O", "check", "--", "first-worker"}
	before := len(o.processes.snapshot())
	// This new helper has the correct private token but belongs to the test
	// parent, not to either registered parent generation. No child may be created.
	cmd := exec.Command(os.Args[0], append([]string{"first-worker-foreign-ssh", o.socket, o.token}, args...)...)
	if err := cmd.Run(); err == nil {
		t.Error("foreign main generation was admitted")
	}
	if len(o.processes.snapshot()) != before {
		t.Error("foreign main generation created an SSH child")
	}
	foreign, err := net.Listen("unix", filepath.Join(shortDir(t), "foreign.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	args[3] = foreign.Addr().String()
	if _, err := o.arguments(args); err == nil {
		t.Error("foreign control socket was admitted")
	}
	connection, err := net.Dial("unix", foreign.Addr().String())
	if err != nil {
		t.Fatal("foreign fixture socket was changed", err)
	}
	connection.Close()
}

func TestFirstWorkerGenerationWaitsForAuthorizedHandler(t *testing.T) {
	f := newFirstWorker(t)
	o := f.ssh
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	o.mu.Lock()
	var control string
	for path := range o.masters {
		control = path
		break
	}
	o.beforeStart = func() { close(arrived); <-release }
	o.mu.Unlock()
	args := []string{"first-worker-ssh", o.socket, o.token, "-F", "/dev/null", "-S", control, "-o", "ControlMaster=no", "-o", "ClearAllForwardings=no", "-o", "ExitOnForwardFailure=yes", "-O", "check", "--", "first-worker"}
	cmd := exec.Command(os.Args[0], args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := make(chan error, 1)
	go func() { helperDone <- cmd.Wait() }()
	defer func() {
		unblock()
		select {
		case <-helperDone:
		case <-time.After(8 * time.Second):
			t.Error("owned broker helper exit not confirmed")
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("authorized handler did not reach registration barrier")
	}
	f.main.stopDaemon()
	cutDone := make(chan error, 1)
	go func() { _, err := o.cutGeneration(); cutDone <- err }()
	returned := false
	select {
	case err := <-cutDone:
		returned = true
		t.Errorf("generation cleanup returned before authorized handler finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if !returned {
		select {
		case err := <-cutDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("generation cleanup did not finish")
		}
	}
	if err := firstWorkerWaitExit(o.processes.snapshot(), nil); err != nil {
		t.Fatal(err)
	}
}

func firstWorkerTree(args []string) int {
	if len(args) != 2 {
		return 2
	}
	depth, err := strconv.Atoi(args[1])
	if err != nil {
		return 2
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	var child *exec.Cmd
	if depth > 0 {
		child = exec.Command(os.Args[0], "first-worker-tree", args[0], strconv.Itoa(depth-1))
		if child.Start() != nil {
			return 2
		}
		defer func() { child.Process.Signal(syscall.SIGTERM); child.Wait() }()
	}
	if err := os.WriteFile(filepath.Join(args[0], args[1]), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return 2
	}
	<-stop
	return 0
}
func TestFirstWorkerDescendantExitEvidence(t *testing.T) {
	o := newFirstWorkerProcesses()
	directory := t.TempDir()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	r, err := o.start(os.Args[0], []string{"first-worker-tree", directory, "2"}, os.Environ(), []*os.File{null, null, null})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.close(); err != nil {
			t.Error(err)
		}
		if err := firstWorkerWaitExit([]*firstWorkerProcess{r}, nil); err != nil {
			t.Error(err)
		}
	})
	eventually(t, "owned child and grandchild ready", 5*time.Second, func() bool { _, err := os.Stat(filepath.Join(directory, "0")); return err == nil })
	children, err := firstWorkerDescendants([]*firstWorkerProcess{r})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("owned descendant tree incomplete: %d", len(children))
	}
	if err := o.close(); err != nil {
		t.Fatal(err)
	}
	if err := firstWorkerWaitExit([]*firstWorkerProcess{r}, children); err != nil {
		t.Fatal(err)
	}
	// A later observation must retain identities captured before root reaping.
	retained, err := firstWorkerDescendants([]*firstWorkerProcess{r})
	if err != nil || len(retained) != 2 {
		t.Fatalf("owned descendant evidence lost: %d %v", len(retained), err)
	}
}

func TestFirstWorkerLateSSHDescendantExitEvidence(t *testing.T) {
	f := newFirstWorker(t)
	o := f.ssh
	before := map[*firstWorkerProcess]bool{}
	for _, record := range o.processes.snapshot() {
		before[record] = true
	}
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	o.mu.Lock()
	o.beforeFreeze = func() { close(arrived); <-release }
	o.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- o.close() }()
	defer func() {
		unblock()
		select {
		case err := <-closed:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("late SSH cleanup not confirmed")
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not reach freeze boundary")
	}
	// This real connection registers both a master and an established server
	// after close has begun, immediately before registration is frozen.
	pool, err := workerssh.New(context.Background(), workerssh.Options{RunDir: filepath.Join(t.TempDir(), "late"), ProbeInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	w, err := pool.Worker(workerssh.Config{Destination: "first-worker", Root: f.root, Binding: f.binding})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "late owned SSH connection established", 10*time.Second, func() bool { return w.Status().Ready })
	var late []*firstWorkerProcess
	for _, record := range o.processes.snapshot() {
		if !before[record] {
			late = append(late, record)
		}
	}
	if len(late) < 2 {
		t.Fatal("late SSH client/server were not registered")
	}
	unblock()
	// The deferred waiter checks close's error; don't consume that channel here.
	eventually(t, "late SSH roots reaped", 10*time.Second, func() bool {
		for _, record := range late {
			select {
			case <-record.done:
			default:
				return false
			}
		}
		return true
	})
	identities, err := firstWorkerDescendants(late)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) == 0 {
		t.Fatal("late SSH descendant evidence missing")
	}
	if err := firstWorkerWaitExit(late, identities); err != nil {
		t.Fatal(err)
	}
}
