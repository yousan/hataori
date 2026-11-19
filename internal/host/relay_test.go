package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func init() {
	// relaystarter <state>: start a relay and keep a connection to it, as
	// the daemon does, until killed.
	extraRoles["relaystarter"] = func(args []string) int {
		if _, err := Start(hostCommand(), args[0], Spec{Relay: true}); err != nil {
			fmt.Println("start:", err)
			return 1
		}
		c, err := Dial(args[0])
		if err != nil {
			fmt.Println("dial:", err)
			return 1
		}
		defer c.Close()
		fmt.Println("started")
		select {}
	}
	// relayanswer <state> <answer>: a new process, as a daemon that started
	// again: it finds the one waiting call and answers it.
	extraRoles["relayanswer"] = func(args []string) int {
		c, err := Dial(args[0])
		if err != nil {
			fmt.Println("dial:", err)
			return 1
		}
		defer c.Close()
		calls, err := c.Waiting()
		if err != nil || len(calls) != 1 {
			fmt.Println("waiting:", calls, err)
			return 1
		}
		if err := c.Answer(calls[0].ID, []byte(args[1])); err != nil {
			fmt.Println("answer:", err)
			return 1
		}
		return 0
	}
}

func startRelay(t *testing.T) string {
	t.Helper()
	return startHost(t, Spec{Relay: true})
}

// The lines a relay writes are the lines a host with a command writes.
func TestRelayRecordsCallsAsToday(t *testing.T) {
	dir := startRelay(t)
	socket := SocketPath(dir)
	payloads := []string{`{"n":1}`, "{\"two\":\n\"lines\"}\n", "\xff not json"}
	for _, p := range payloads {
		if _, err := Call(socket, []byte(p), false, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	lines := bytes.Split(bytes.TrimSuffix(read(t, dir, EventsFile), []byte("\n")), []byte("\n"))
	if len(lines) != len(payloads) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(payloads), read(t, dir, EventsFile))
	}
	for i, l := range lines {
		var head struct {
			Hook string `json:"hook"`
			Wait bool   `json:"wait"`
			At   int64  `json:"at"`
		}
		// The payload may not be JSON; the head is read from a copy that is.
		head0 := bytes.Clone(l[:bytes.Index(l, []byte(`,"payload":`))])
		jsonable := append(head0, '}')
		if err := json.Unmarshal(jsonable, &head); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		want := CallLine(head.Hook, false, time.UnixMilli(head.At), []byte(payloads[i]))
		if !bytes.Equal(l, want) || head.Hook == "" {
			t.Fatalf("line %d is %q, want %q", i, l, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Fatalf("a relay wrote %s: %v", PIDFile, err)
	}
}

// A waiting call is listed, held until it is answered, and dropped when its
// hook goes away. Nothing reaches a terminal: there is none.
func TestRelayHoldsAndListsWaitingCalls(t *testing.T) {
	dir := startRelay(t)
	socket := SocketPath(dir)
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	done := make(chan []byte, 1)
	go func() {
		out, code := runHook(socket, []byte(`{"ask":1}`), "--wait", "--timeout", "20s")
		done <- append(out, fmt.Sprint(" exit=", code)...)
	}()
	var calls []WaitingCall
	waitFor(t, "the waiting call", func() bool { calls, _ = c.Waiting(); return len(calls) == 1 })
	if string(calls[0].Payload) != `{"ask":1}` {
		t.Fatalf("waiting call %+v", calls[0])
	}
	if lines := hookLines(t, dir); len(lines) != 1 || !lines[0].Wait || lines[0].Hook != calls[0].ID {
		t.Fatalf("events.ndjson: %+v, listed %+v", lines, calls)
	}
	select {
	case out := <-done:
		t.Fatalf("the hook ended before an answer: %q", out)
	case <-time.After(300 * time.Millisecond):
	}
	if err := c.Answer(calls[0].ID, []byte("allow")); err != nil {
		t.Fatal(err)
	}
	if out := <-done; string(out) != "allow exit=0" {
		t.Fatalf("hook printed %q", out)
	}

	// The hook gives up: the call is no longer listed.
	if _, code := runHook(socket, []byte(`{"ask":2}`), "--wait", "--timeout", "300ms"); code != HookTimeout {
		t.Fatalf("hook exited %d", code)
	}
	waitFor(t, "the call to go", func() bool { calls, _ = c.Waiting(); return len(calls) == 0 })

	if err := c.Input([]byte("x")); err == nil {
		t.Fatal("input to a relay was accepted")
	}
	if pid, err := c.PID(); err != nil || pid != 0 {
		t.Fatalf("PID of a relay: %d, %v", pid, err)
	}
	if _, err := FollowTerminal(dir, "", 0); err == nil {
		t.Fatal("a relay's terminal could be followed")
	}
}

// The daemon is killed while a call waits: the call still waits, and the
// daemon started again answers it.
func TestRelayCallOutlivesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { killGroups(dir) })
	daemon := exec.Command(os.Args[0], "role", "relaystarter", dir)
	out, _ := daemon.StdoutPipe()
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "started\n" {
		daemon.Process.Kill()
		daemon.Wait()
		t.Fatalf("daemon: %q", line)
	}

	done := make(chan []byte, 1)
	go func() {
		out, code := runHook(SocketPath(dir), []byte(`{"ask":"across"}`), "--wait", "--timeout", "30s")
		done <- append(out, fmt.Sprint(" exit=", code)...)
	}()
	waitFor(t, "the call", func() bool { return len(hookLines(t, dir)) == 1 })

	daemon.Process.Signal(syscall.SIGKILL)
	daemon.Wait()
	select {
	case out := <-done:
		t.Fatalf("the hook ended when the daemon died: %q", out)
	case <-time.After(500 * time.Millisecond):
	}

	again := exec.Command(os.Args[0], "role", "relayanswer", dir, "answered after the restart")
	if b, err := again.CombinedOutput(); err != nil {
		t.Fatalf("the daemon started again could not answer: %v: %s", err, b)
	}
	if got := <-done; string(got) != "answered after the restart exit=0" {
		t.Fatalf("hook printed %q", got)
	}
}

// Told to stop, a relay signals nothing, records that it ran no command and
// ends; a call still waiting is let go.
func TestRelayStop(t *testing.T) {
	dir := startRelay(t)
	code := make(chan int, 1)
	go func() {
		_, c := runHook(SocketPath(dir), []byte(`{}`), "--wait", "--timeout", "20s")
		code <- c
	}()
	waitFor(t, "the call", func() bool { return len(hookLines(t, dir)) == 1 })

	r, err := Stop(dir, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Signals != "" || r.Already {
		t.Fatalf("stop of a relay: %+v", r)
	}
	if st := r.Status; !st.Relay || !st.Stopped || st.Exited || st.Error != "" || st.StopNote != "" {
		t.Fatalf("status of a relay: %+v", st)
	}
	if !Gone(dir) {
		t.Fatal("Gone is false after the relay was stopped")
	}
	if c := <-code; c != HookGone {
		t.Fatalf("the waiting hook exited %d", c)
	}
	// The host is this test's child, so it stays a zombie: its own record
	// says that Run returned.
	waitFor(t, "the host to record its end", func() bool {
		return bytes.HasSuffix(read(t, dir, HostLog), []byte(" ended\n"))
	})
	// A second stop finds nothing running.
	if r, err := Stop(dir, time.Second, time.Second); err != nil || !r.Already || !r.Status.Relay {
		t.Fatalf("second stop: %+v, %v", r, err)
	}
}

func TestRelayRefusesACommand(t *testing.T) {
	for _, spec := range []Spec{{Relay: true, Argv: self("echo")}, {Relay: true, Terminal: true}} {
		if _, err := Start(hostCommand(), t.TempDir(), spec); !errors.Is(err, errRelayCommand) {
			t.Fatalf("Start(%+v): %v", spec, err)
		}
	}
}
