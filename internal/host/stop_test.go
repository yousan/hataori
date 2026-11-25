package host

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	// tree <depth>: ignore SIGINT, SIGTERM and SIGHUP, start a child one
	// level down, report, and wait forever.
	extraRoles["tree"] = func(args []string) int {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		depth, _ := strconv.Atoi(args[0])
		if depth > 0 {
			child := exec.Command(os.Args[0], "role", "tree", strconv.Itoa(depth-1))
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				fmt.Println("start child:", err)
			}
		}
		fmt.Printf("up %d\n", depth)
		// Not select {}: with nothing else to run, a Go program built
		// without cgo (as on macOS) ends there with "all goroutines are
		// asleep", exit code 2.
		for {
			time.Sleep(time.Hour)
		}
	}
	// lastwords: on SIGINT, write a last line and exit 130.
	extraRoles["lastwords"] = func([]string) int {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		fmt.Println("ready")
		<-ch
		fmt.Println("last words")
		return 130
	}
}

func commandPID(t *testing.T, dir string) int {
	t.Helper()
	var pid int
	waitFor(t, "the command PID", func() bool {
		var err error
		pid, err = strconv.Atoi(strings.TrimSpace(string(read(t, dir, PIDFile))))
		return err == nil && pid > 0
	})
	return pid
}

func output(t *testing.T, dir string, terminal bool) []byte {
	if terminal {
		return read(t, dir, TerminalLog)
	}
	return read(t, dir, EventsFile)
}

func TestStopLeavesNoProcess(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			dir := startHost(t, Spec{Argv: self("tree", "2"), Env: []string{}, Terminal: terminal})
			waitFor(t, "the grandchild", func() bool { return bytes.Contains(output(t, dir, terminal), []byte("up 0")) })
			pgid := commandPID(t, dir)

			r, err := Stop(dir, 50*time.Millisecond, 50*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if r.Signals != "SIGINT,SIGTERM,SIGKILL" || !r.Status.Stopped || r.Status.Signal != "killed" {
				t.Fatalf("stop: %+v", r)
			}
			if !groupGone(pgid) {
				t.Fatal("processes of the command's group are left")
			}
		})
	}
}

func TestStopEndsAtInterrupt(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			dir := startHost(t, Spec{Argv: self("lastwords"), Env: []string{}, Terminal: terminal})
			waitFor(t, "the command", func() bool { return bytes.Contains(output(t, dir, terminal), []byte("ready")) })
			r, err := Stop(dir, 5*time.Second, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if r.Signals != "SIGINT" || r.Status.Code != 130 || !r.Status.Stopped {
				t.Fatalf("stop: %+v", r)
			}
			// The logs hold everything the command wrote, up to its end.
			if !bytes.Contains(output(t, dir, terminal), []byte("last words")) {
				t.Fatalf("the last line is missing: %q", output(t, dir, terminal))
			}
		})
	}
}

func TestStopAfterExit(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("exit", "0"), Env: []string{}})
	waitFor(t, "the end", func() bool { return Gone(dir) })
	r, err := Stop(dir, time.Second, time.Second)
	if err != nil || !r.Already || r.Signals != "" || !r.Status.Exited {
		t.Fatalf("stop after exit: %+v, %v", r, err)
	}
}

func TestStopFindsNoSocketFile(t *testing.T) {
	// A state directory that never had a host: Stop says so instead of
	// reporting success.
	if _, err := Stop(filepath.Join(t.TempDir(), "none"), time.Second, time.Second); err == nil {
		t.Fatal("Stop without a host succeeded")
	}
}

// A host that ends removes its socket and then records the end. A Stop that
// falls between the two finds neither, and still answers with the end.
func TestStopWhileTheEndIsRecorded(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) {
		t.Helper()
		if err := writeFile(filepath.Join(dir, name), s); err != nil {
			t.Fatal(err)
		}
	}
	// A host that runs, with no socket; it records its end once Stop, which
	// has found none, has looked whether it runs.
	pid := filepath.Join(dir, HostPIDFile)
	if err := syscall.Mkfifo(pid, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, err := os.OpenFile(pid, os.O_WRONLY, 0) // until Stop reads it
		if err != nil {
			return // Stop did not read it
		}
		self := strconv.Itoa(os.Getpid())
		if err := errors.Join(writeFile(pid+".new", self), os.Rename(pid+".new", pid)); err != nil { // for its later reads
			t.Error(err)
		}
		f.WriteString(self)
		f.Close()
		time.Sleep(50 * time.Millisecond)
		writeStatus(dir, Status{Exited: true, Stopped: true})
	}()
	r, err := Stop(dir, time.Second, time.Second)
	if err != nil || !r.Already || !r.Status.Stopped {
		t.Fatalf("stop as the end is recorded: %+v, %v", r, err)
	}

	// One that went without recording its end records none any more.
	gone := exec.Command(os.Args[0], "role", "exit", "0")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, StatusFile))
	write(HostPIDFile, strconv.Itoa(gone.Process.Pid))
	from := time.Now()
	if _, err := Stop(dir, time.Second, time.Second); err == nil || time.Since(from) > endingWait/2 {
		t.Fatalf("stop with a host that is gone: %v after %v, want an error at once", err, time.Since(from))
	}
}
