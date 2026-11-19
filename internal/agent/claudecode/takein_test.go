package claudecode

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/window"
)

// A session taken in is past its start: its codec reports it ready at
// Open, watches no screen, and pastes the first input at once. Rebuilt
// after a restart, it is ready without being opened.
func TestTakenInCodecStartsReady(t *testing.T) {
	a := pluginAdapter(t)
	spec := agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir(), ResumeID: "00000000-0000-4000-8000-000000000001",
		Model: "claude-haiku-5-5", TakenIn: true}
	c := a.NewCodec(spec)
	open := c.Open()
	if open.WatchScreen || len(open.Events) != 1 || open.Events[0].Kind != agent.SessionReady ||
		open.Events[0].SessionID != spec.ResumeID || open.Events[0].Model != spec.Model {
		t.Fatalf("Open: %+v", open)
	}
	for _, c := range []agent.Codec{c, a.NewCodec(spec)} { // opened, and rebuilt
		in, err := c.Input(agent.Input{ID: "7", Text: "hello"})
		if err != nil || len(in.Keys) != 1 || in.Keys[0].Paste != "hello" {
			t.Fatalf("the first input is not pasted at once: %+v, %v", in, err)
		}
	}
	// An ordinary session still watches its start.
	spec.TakenIn = false
	if open := a.NewCodec(spec).Open(); !open.WatchScreen || len(open.Events) != 0 {
		t.Fatalf("Open of a session hataori starts: %+v", open)
	}
}

// relayHost starts a session host with no command in a directory of its
// own, as a take-in does.
func relayHost(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { host.Stop(dir, time.Second, time.Second); killGroups(dir) })
	if _, err := host.Start(host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, dir, host.Spec{Relay: true}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// takenRoot is an instance root whose window of TMUX "/tmp/tmux-1000/default,4242,0"
// and pane %7 was taken in for process 4242, its calls going to relay.
func takenRoot(t *testing.T, relay string) string {
	t.Helper()
	root := t.TempDir()
	rec := window.Record{Server: "/tmp/tmux-1000/default,4242", Pane: "%7", PID: 4242, Socket: host.SocketPath(relay), Thread: 3, Session: 4}
	if err := window.WriteRecord(root, rec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		window.RemoveRecord(root, rec.Server, rec.Pane)
		sock, _ := window.SocketPath(root)
		os.Remove(filepath.Join(filepath.Dir(sock), window.TakenDir))
		os.Remove(filepath.Dir(sock)) // a short path under /tmp, when the root's is too long
	})
	return root
}

// In a window taken in, with no daemon at all, the hook of the process
// that was taken in goes to the thread's relay host as `hataori hook`
// does: recorded there, and a waiting one answered from there. The hook of
// another process in the same pane is only reported.
func TestWindowHookOfATakenInWindow(t *testing.T) {
	relay := relayHost(t)
	root := takenRoot(t, relay)
	env := map[string]string{"TMUX": "/tmp/tmux-1000/default,4242,0", "TMUX_PANE": "%7", "CLAUDE_CODE_ENTRYPOINT": "cli"}
	var sent []window.Report
	hook := func(pid int, out io.Writer) windowHook {
		return windowHook{getenv: func(k string) string { return env[k] }, ppid: pid + 1, stdout: out,
			terminal: func(context.Context, int) (int, string, bool, error) { return pid, "/dev/pts/9", true, nil },
			send:     func(_ context.Context, _ string, r window.Report) error { sent = append(sent, r); return nil }}
	}
	payload := capturedPayload(t, "2.1.327/confine_files_settings_flag/hooks/02-UserPromptSubmit.json")

	var out bytes.Buffer
	if code := hook(4242, &out).main([]string{"--window", "--root", root}, bytes.NewReader(payload), io.Discard); code != host.HookOK || out.Len() != 0 {
		t.Fatalf("a hook of the process taken in: exit %d, printed %q", code, out.String())
	}
	if ev := read(t, relay); !strings.Contains(ev, `"hook_event_name":"UserPromptSubmit"`) || len(sent) != 0 {
		t.Fatalf("not relayed: events.ndjson %q, reported %d", ev, len(sent))
	}

	// A waiting call is answered from the relay host.
	done := make(chan int, 1)
	out.Reset()
	go func() {
		done <- hook(4242, &out).main([]string{"--window", "--root", root, "--wait", "--timeout", "20s"},
			bytes.NewReader(capturedPayload(t, "2.1.327/usage/hooks/05-PostToolUse.json")), io.Discard)
	}()
	c, err := host.Dial(relay)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var calls []host.WaitingCall
	for deadline := time.Now().Add(10 * time.Second); len(calls) == 0; time.Sleep(10 * time.Millisecond) {
		if calls, _ = c.Waiting(); time.Now().After(deadline) {
			t.Fatal("the waiting call never reached the relay host")
		}
	}
	if err := c.Answer(calls[0].ID, []byte(`{"decision":"allow"}`)); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != host.HookOK || out.String() != `{"decision":"allow"}` {
		t.Fatalf("the waiting hook: exit %d, printed %q", code, out.String())
	}

	// Another process in the pane, such as a `claude -p` its shell started.
	before := read(t, relay)
	if code := hook(5555, io.Discard).main([]string{"--window", "--root", root}, bytes.NewReader(payload), io.Discard); code != host.HookOK {
		t.Fatalf("another process's hook: exit %d", code)
	}
	if read(t, relay) != before || len(sent) != 1 || sent[0].PID != 5555 {
		t.Fatalf("another process's hook was relayed, or not reported: %d reports", len(sent))
	}
}

func read(t *testing.T, relay string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(relay, host.EventsFile))
	return string(b)
}
