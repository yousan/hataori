package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/wire"
)

// setConfig changes root's config with f.
func setConfig(t *testing.T, root string, f func(cfg map[string]any)) {
	t.Helper()
	p := filepath.Join(root, instance.ConfigFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	f(cfg)
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// onlyThread returns the one thread Threads lists once state is s and it
// has had a turn.
func onlyThread(t *testing.T, c *wire.Client, s control.State) control.ThreadInfo {
	t.Helper()
	var ts []control.ThreadInfo
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var err error
		if ts, err = c.Threads(context.Background(), control.Filter{}); err != nil {
			t.Fatal(err)
		}
		if len(ts) == 1 && ts[0].State == s && ts[0].Turns > 0 {
			return ts[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the thread never became %s: %+v", s, ts)
		}
	}
}

// commandTTY is the terminal the session's command holds, as ps reports
// it: what the session host opened for it, read from outside hataori.
func commandTTY(t *testing.T, root string) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	if len(dirs) != 1 {
		t.Fatalf("%d session directories", len(dirs))
	}
	b, err := os.ReadFile(filepath.Join(dirs[0], host.PIDFile))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	tty := strings.TrimSpace(string(out))
	if tty == "" || tty == "?" || tty == "??" {
		t.Fatalf("the command (pid %d) holds no terminal: %q", pid, tty)
	}
	return "/dev/" + tty
}

// A thread whose session runs in a terminal gives the pty its session
// host opened, in Threads and Thread, also to a caller that may not read
// text; HostTerminals still leaves that terminal out.
func TestThreadTerminalIsTheSessionHostsPty(t *testing.T) {
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = true })
	dashboardHook = func(c *control.Caller) { c.Scopes, c.NoText = []string{"s"}, true }
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	got := onlyThread(t, c, control.Idle)
	want := commandTTY(t, root)
	if got.Terminal != want {
		t.Fatalf("Threads: terminal %q, the command holds %q", got.Terminal, want)
	}
	d, err := c.Thread(ctx, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Terminal != want {
		t.Fatalf("Thread: terminal %q, want %q", d.Terminal, want)
	}

	// The dashboard's caller may not read text: no last line, but the terminal.
	base, cookie := dashSignIn(t, root)
	code, body := dashGet(t, base, cookie, "/api/threads")
	var ts []map[string]any
	if err := json.Unmarshal([]byte(body), &ts); code != 200 || err != nil || len(ts) != 1 {
		t.Fatalf("/api/threads: %d %s", code, body)
	}
	if _, ok := ts[0]["last"]; ok || ts[0]["terminal"] != want {
		t.Fatalf("/api/threads for a caller that may not read text: %s", body)
	}

	hts, err := c.HostTerminals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hts {
		if h.Device == want {
			t.Fatalf("HostTerminals lists the session's terminal %s: %+v", want, h)
		}
	}
}

// An asleep thread has no terminal.
func TestAsleepThreadHasNoTerminal(t *testing.T) {
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["idle"] = "1s" })
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, err := c.Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := onlyThread(t, c, control.Asleep); got.Terminal != "" {
		t.Fatalf("Threads: an asleep thread has terminal %q", got.Terminal)
	}
	if d, err := c.Thread(context.Background(), id, 0, 0); err != nil || d.Terminal != "" {
		t.Fatalf("Thread: an asleep thread has terminal %q (%v)", d.Terminal, err)
	}
}

// A thread whose session runs on pipes has no terminal, while its agent
// runs.
func TestThreadOnPipesHasNoTerminal(t *testing.T) {
	root := newRoot(t)
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id, err := c.Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	got := onlyThread(t, c, control.Idle)
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	if len(dirs) != 1 || host.Gone(dirs[0]) {
		t.Fatalf("the session's host does not run: %v", dirs)
	}
	if got.Terminal != "" {
		t.Fatalf("Threads: a session on pipes has terminal %q", got.Terminal)
	}
	if d, err := c.Thread(context.Background(), id, 0, 0); err != nil || d.Terminal != "" {
		t.Fatalf("Thread: a session on pipes has terminal %q (%v)", d.Terminal, err)
	}
}
