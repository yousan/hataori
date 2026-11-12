package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	extraRoles["hook"] = func(args []string) int {
		return HookMain(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	}
}

// hookLine is a call as the host wrote it.
type hookLine struct {
	Hook    string          `json:"hook"`
	Wait    bool            `json:"wait"`
	Payload json.RawMessage `json:"payload"`
}

func hookLines(t *testing.T, dir string) []hookLine {
	t.Helper()
	var out []hookLine
	for _, l := range bytes.Split(bytes.TrimSuffix(read(t, dir, EventsFile), []byte("\n")), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var h hookLine
		if err := json.Unmarshal(l, &h); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, h)
	}
	return out
}

func hookHost(t *testing.T) (dir, socket string) {
	dir = startHost(t, Spec{Argv: self("echo"), Env: []string{}})
	return dir, SocketPath(dir)
}

// runHook runs `hataori hook` (the test binary's hook role) as an agent
// would, with the host's socket in its environment.
func runHook(socket string, stdin []byte, args ...string) (stdout []byte, code int) {
	cmd := exec.Command(os.Args[0], append([]string{"role", "hook"}, args...)...)
	cmd.Env = []string{HookEnvVar + "=" + socket}
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode()
	}
	return out, 0
}

func TestFiftyCallsAreFiftyLines(t *testing.T) {
	dir, socket := hookHost(t)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("p", 8000))
			if _, err := Call(socket, []byte(p+"\n"), false, 5*time.Second); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	lines := hookLines(t, dir)
	if len(lines) != 50 {
		t.Fatalf("%d lines, want 50", len(lines))
	}
	ids := map[string]bool{}
	for _, l := range lines {
		ids[l.Hook] = true
		var p struct{ N int }
		if json.Unmarshal(l.Payload, &p) != nil || !bytes.HasSuffix(l.Payload, []byte(strings.Repeat("p", 8000)+`"}`)) {
			t.Fatalf("payload changed: %.60q", l.Payload)
		}
	}
	if len(ids) != 50 {
		t.Fatalf("%d distinct ids", len(ids))
	}
}

func TestWaitPrintsTheAnswer(t *testing.T) {
	dir, socket := hookHost(t)
	answer := []byte("{\"decision\":\"allow\"}\n\xff raw\x00bytes")
	type result struct {
		out  []byte
		code int
	}
	done := make(chan result, 1)
	go func() {
		out, code := runHook(socket, []byte(`{"q":1}`), "--wait", "--timeout", "10s")
		done <- result{out, code}
	}()

	waitFor(t, "the call to wait", func() bool { return len(hookLines(t, dir)) == 1 })
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Answer("not-a-call", []byte("x")); err == nil {
		t.Fatal("an answer for an id that is not waiting was accepted")
	}
	if err := c.Answer(hookLines(t, dir)[0].Hook, answer); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.code != HookOK || !bytes.Equal(r.out, answer) {
		t.Fatalf("hook printed %q and exited %d", r.out, r.code)
	}
	if err := c.Answer(hookLines(t, dir)[0].Hook, answer); err == nil {
		t.Fatal("a second answer to the same call was accepted")
	}
}

func TestAnswerFromANewClient(t *testing.T) {
	dir, socket := hookHost(t)
	done := make(chan []byte, 1)
	go func() {
		out, _ := runHook(socket, []byte(`{"q":2}`), "--wait", "--timeout", "10s")
		done <- out
	}()
	var calls []WaitingCall
	first, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the waiting call", func() bool { calls, _ = first.Waiting(); return len(calls) == 1 })
	first.Close() // the client that would answer goes away

	second, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	calls, err = second.Waiting()
	if err != nil || len(calls) != 1 || string(calls[0].Payload) != `{"q":2}` {
		t.Fatalf("waiting calls seen by a new client: %+v, %v", calls, err)
	}
	if err := second.Answer(calls[0].ID, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if out := <-done; string(out) != "ok" {
		t.Fatalf("hook printed %q", out)
	}
}

func TestHookWithoutAnswer(t *testing.T) {
	_, socket := hookHost(t)
	out, code := runHook(socket, []byte(`{}`), "--wait", "--timeout", "200ms")
	if code != HookTimeout || len(out) != 0 {
		t.Fatalf("deadline: printed %q, exited %d", out, code)
	}
	out, code = runHook(socket+".none", []byte(`{}`), "--wait")
	if code != HookGone || len(out) != 0 {
		t.Fatalf("no host: printed %q, exited %d", out, code)
	}

	// The host goes away while the hook waits.
	dir, socket := hookHost(t)
	done := make(chan int, 1)
	go func() { _, code := runHook(socket, []byte(`{}`), "--wait", "--timeout", "10s"); done <- code }()
	waitFor(t, "the call", func() bool { return len(hookLines(t, dir)) == 1 })
	if _, err := Stop(dir, time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != HookGone {
		t.Fatalf("hook exited %d after its host went away", code)
	}
}

func TestQuickCallBesideFiftyWaiting(t *testing.T) {
	dir, socket := hookHost(t)
	for range 50 {
		go Call(socket, []byte(`{"wait":true}`), true, 20*time.Second)
	}
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "50 waiting calls", func() bool { w, _ := c.Waiting(); return len(w) == 50 })
	start := time.Now()
	if _, err := Call(socket, []byte(`{"quick":true}`), false, time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("a call without --wait took %v", d)
	}
}
