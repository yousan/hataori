package worker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/machineio"
)

func TestWorkerLoginGuardEndsDescendantAfterCleanLeaderExit(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a := &loginTestAdapter{mode: "descendant", health: agent.Health{Installed: true, LoggedIn: true}, extraEnv: []string{"WORKER_LOGIN_LIFETIME=" + listener.Addr().String()}}
	f, err := startLoginAttempt(context.Background(), a, agent.Spec{Dir: t.TempDir()}, helperArgv("guard"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer f.stop()
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	c, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() // also cleans up the intentionally orphaned negative control
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(c)
	if line, err := reader.ReadString('\n'); err != nil || line != "alive\n" {
		t.Fatal("descendant did not establish its owned lifetime socket")
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatal("guard left a descendant alive after its parent exited")
	}
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("guarded completion remained blocked")
	}
}

func TestWorkerLoginOwnerDeathClosesGuard(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	argv := helperArgv("owned-owner")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"WORKER_LOGIN_HELPER=1", "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0", "WORKER_LOGIN_LIFETIME=" + listener.Addr().String(), "WORKER_LOGIN_DIR=" + t.TempDir()}
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("could not start disposable owner process")
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }() // only this fixture's still-owned command
	if !bufio.NewScanner(out).Scan() {
		t.Fatal("disposable owner did not start login")
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	c, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() // also ends the negative control without any PID lookup
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(c)
	if line, err := reader.ReadString('\n'); err != nil || line != "alive\n" {
		t.Fatal("login child did not establish its lifetime socket")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatal("login child survived abrupt owner death")
	}
}

type blockedLoginInput struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
	io.WriteCloser
}

func (w *blockedLoginInput) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.ctx.Done()
	return 0, errors.New("isolated blocked writer cancelled")
}

func TestWorkerLoginDisconnectInterruptsBlockedInput(t *testing.T) {
	f, _, open, _ := loginResident(t, "code")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := machineio.StartLogin(ctx, f.binding, open, f.configuration, f.options.Config.Agents[0].ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for {
		d, err := l.Next()
		if err != nil {
			t.Fatal(err)
		}
		if d.WantsCode {
			break
		}
	}
	f.r.mu.Lock()
	attempt := f.r.loginAttempts[f.configuration]
	attempt.mu.Lock()
	w := &blockedLoginInput{ctx: attempt.ctx, started: make(chan struct{}), WriteCloser: attempt.stdin}
	attempt.stdin = w // deterministic stand-in for a CLI whose stdin pipe is full
	attempt.mu.Unlock()
	f.r.mu.Unlock()
	if l.Code("marker-secret") != nil {
		t.Fatal("could not send fixture input")
	}
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("fixture did not block in stdin write")
	}
	l.Close()
	select {
	case <-attempt.done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not interrupt blocked CLI input")
	}
}

func TestWorkerLoginCancellationAcknowledgesCleanupBeforeFreshStart(t *testing.T) {
	f, _, open, count := loginResident(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := machineio.StartLogin(ctx, f.binding, open, f.configuration, f.options.Config.Agents[0].ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Next(); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, err := l.Next(); err != nil {
				return
			}
		}
	}()
	if l.Cancel() != nil {
		t.Fatal("cleanup acknowledgement was not received")
	}
	<-readDone
	// There is deliberately no polling/sleep between acknowledgement and start.
	next, err := machineio.StartLogin(ctx, f.binding, open, f.configuration, f.options.Config.Agents[0].ConfigDir)
	if err != nil {
		t.Fatal("acknowledged cancellation still held the configuration")
	}
	next.Close()
	if count.Load() != 2 {
		t.Fatal("replacement did not open exactly one fresh connection")
	}
}

func TestWorkerLoginMismatchedDirectoryNeverStartsCommand(t *testing.T) {
	f, a, open, _ := loginResident(t, "code")
	l, err := machineio.StartLogin(context.Background(), f.binding, open, f.configuration, "another-directory")
	if err == nil {
		l.Close()
		t.Fatal("wrong-directory outage started a command")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.launch.ConfigDir != "" {
		t.Fatal("mismatched directory reached a login process")
	}
}

func TestWorkerLoginOutputGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &loginAttempt{ctx: ctx, cancel: cancel, login: &loginTestAdapter{}, updates: make(chan loginUpdate, 16)}
	w := &loginOutput{flow: f}
	if _, err := w.Write([]byte(strings.Repeat("x", loginLineLimit+1))); err == nil || ctx.Err() == nil {
		t.Fatal("oversized output was accepted")
	}
}

func TestWorkerLoginGuardDescriptorsArePrivate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	launch := agent.Launch{Argv: []string{"/bin/sh", "-c", `if ( : <&3 ) 2>/dev/null; then exit 9; fi; if ( : >&4 ) 2>/dev/null; then exit 9; fi; exit 0`}, Env: []string{"WORKER_LOGIN_HELPER=1", "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0"}}
	cmd, err := startLoginCommand(ctx, launch, t.TempDir(), helperArgv("guard"), io.Discard)
	if err != nil || !cmd.wait(ctx) {
		t.Fatal("guard descriptors reached the CLI")
	}
}

func TestWorkerLoginGuardEndsDirectChildThatChangesSession(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a := &loginTestAdapter{mode: "detached-lifetime", extraEnv: []string{"WORKER_LOGIN_LIFETIME=" + listener.Addr().String()}}
	f, err := startLoginAttempt(context.Background(), a, agent.Spec{Dir: t.TempDir()}, helperArgv("guard"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer f.stop()
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	c, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() // negative control exits without any PID-based cleanup
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(c)
	if line, err := reader.ReadString('\n'); err != nil || line != "alive\n" {
		t.Fatal("detached direct child did not establish its lifetime socket")
	}
	f.stop()
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatal("direct login child survived by changing session")
	}
}

func TestWorkerLoginEmptyEnvironmentDoesNotInherit(t *testing.T) {
	// The parent is itself isolated before the negative control can exercise
	// exec's nil-environment inheritance. No ambient credential is exposed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	argv := helperArgv("empty-env-owner")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"WORKER_LOGIN_HELPER=1", "WORKER_LOGIN_FORBIDDEN=fixture-only", "WORKER_LOGIN_DIR=" + t.TempDir(), "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0"}
	if err := cmd.Run(); err != nil {
		t.Fatal("empty login environment inherited the parent's fixture marker")
	}
}
