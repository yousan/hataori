package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/runner"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// windowCheckWait is longer than the runner asks the window's holder.
const windowCheckWait = 3 * time.Second

// takeInRoot is an instance with host terminals on and the fake agent in a
// terminal, whose launches by hataori record their arguments, environment
// and directory in the directory it returns.
func takeInRoot(t *testing.T) (*chatRoot, string) {
	t.Helper()
	record := t.TempDir()
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["host_terminals"] = true
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Terminal: true, Hataori: os.Args[0], Record: record}}
	})
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = takeSocket() }
	t.Cleanup(func() { hostTermsHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	return r, record
}

// takeIn takes the rig's window in to scope c1 and returns the thread and
// its session, once the session is idle.
func takeIn(t *testing.T, r *chatRoot, g *takeRig) (control.ThreadID, int64) {
	t.Helper()
	var out string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		code, o, errOut := cli(t, "take", g.window, "--scope", "c1", "--key", "take-1", "--root", r.root)
		if code == 0 {
			out = o
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not taken in: %s", errOut)
		}
	}
	var took struct{ Thread control.ThreadID }
	if err := json.Unmarshal([]byte(out), &took); err != nil {
		t.Fatal(err)
	}
	var sid int64
	eventually(t, "the session idle", 15*time.Second, func() bool {
		var state string
		r.db(t).QueryRow(`SELECT id, state FROM sessions WHERE thread_id = ?`, int64(took.Thread)).Scan(&sid, &state)
		return state == "idle"
	})
	return took.Thread, sid
}

// closedWindow waits until the session is asleep with the reason "window
// closed", and its relay host has ended.
func closedWindow(t *testing.T, r *chatRoot, id control.ThreadID, sid int64) {
	t.Helper()
	eventually(t, "asleep, the window closed", 15*time.Second, func() bool {
		var n int
		r.db(t).QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'state'
			AND json_extract(CAST(data AS TEXT), '$.to') = 'asleep' AND json_extract(CAST(data AS TEXT), '$.reason') = 'window closed'`, int64(id)).Scan(&n)
		return n == 1
	})
	dir := runner.StateDir(r.root, sid)
	eventually(t, "the relay host to end", 10*time.Second, func() bool { return host.Gone(dir) })
}

// tree is every file and directory under dir, with its mode and content.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		st, _ := d.Info()
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %v %d\n", rel, st.Mode(), st.ModTime().UnixNano())
		if st.Mode().IsRegular() {
			c, _ := os.ReadFile(p)
			b.Write(c)
			b.WriteByte('\n')
		}
		return nil
	})
	return b.String()
}

// The acceptance criteria of #401, the window's agent ending:
//   - its SessionEnd puts the thread asleep with reason "window closed" and
//     ends the relay host;
//   - a SessionStart of the same session in another window lists that
//     window with the thread's number and does not take it in;
//   - the next message resumes the session under hataori's own host, in the
//     window's directory, with the model the session last reported and the
//     configuration directory the window used;
//   - removing the thread removes its state directory and leaves the
//     directory, with an untracked file and an unpushed commit, as it was.
func TestTakenInWindowClosesAndTheThreadGoesOn(t *testing.T) {
	r, record := takeInRoot(t)
	config := t.TempDir() // the window's own: logged in
	if err := os.WriteFile(filepath.Join(config, fake.LoggedInFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newTakeRigWith(t, r.root, false, "--config "+config)
	// The person's directory holds work of theirs that is nowhere else.
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=person", "-c", "user.email=person@example.com"}, args...)...)
		cmd.Dir = g.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(g.dir, "kept.txt"), []byte("committed\n"), 0o600)
	git("add", "kept.txt")
	git("commit", "-q", "-m", "only here")
	os.WriteFile(filepath.Join(g.dir, "untracked.txt"), []byte("not added\n"), 0o600)
	before := tree(t, g.dir)
	// A window of the test's that keeps the server when the agent's goes.
	g.tmux(t, "new-window", "-d", "-t", "work", "sleep 600")

	id, sid := takeIn(t, r, g)
	th := r.thread(t, 0)

	// A /clear ends the session's SessionEnd too, and the agent goes on:
	// the window stays taken in.
	g.tmux(t, "send-keys", "-t", g.pane, "-l", "session-end clear")
	g.tmux(t, "send-keys", "-t", g.pane, "Enter")
	eventually(t, "the clear's turn to end", 15*time.Second, func() bool { return len(r.events(t, "turn_ended", th)) == 1 })
	time.Sleep(windowCheckWait)
	var still string
	r.db(t).QueryRow(`SELECT state FROM sessions WHERE id = ?`, sid).Scan(&still)
	if _, err := os.Stat(filepath.Join(runner.StateDir(r.root, sid), runner.TakenFile)); err != nil || still == "asleep" {
		t.Fatalf("a /clear closed the window: the session is %s, %v", still, err)
	}

	// The person ends the session in the window. Its SessionEnd comes while
	// its process still runs there, so the holder still finds it: only the
	// hook closes the window here.
	g.tmux(t, "send-keys", "-t", g.pane, "-l", "session-end")
	g.tmux(t, "send-keys", "-t", g.pane, "Enter")
	closedWindow(t, r, id, sid)
	if !alive(g.pid) {
		t.Fatal("the agent's process ended: the check would not tell the hook from the holder")
	}

	// The same session in another window: listed with the thread's number,
	// not taken in.
	pane2 := g.tmux(t, "new-window", "-P", "-F", "#{pane_id}", "-t", "work", "-c", g.dir, g.agent)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	eventually(t, "the other window listed with the thread", 15*time.Second, func() bool {
		ts, err := c.HostTerminals(t.Context())
		if err != nil {
			return false
		}
		return slices.ContainsFunc(ts, func(h control.HostTerminal) bool {
			return h.Tmux != nil && h.Tmux.Pane == pane2 && h.Reported != nil && h.Reported.Thread != nil && *h.Reported.Thread == id
		})
	})
	var state string
	r.db(t).QueryRow(`SELECT state FROM sessions WHERE id = ?`, sid).Scan(&state)
	if state != "asleep" {
		t.Fatalf("the other window's start changed the session: %s", state)
	}
	if _, err := os.Stat(filepath.Join(runner.StateDir(r.root, sid), runner.TakenFile)); err == nil {
		t.Fatal("the other window was taken in")
	}
	g.tmux(t, "kill-pane", "-t", pane2)

	// The next message resumes it under hataori's own host.
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "reply resumed")
	r.posted(t, th, "resumed")
	starts, _ := filepath.Glob(filepath.Join(record, "start-*.json"))
	if len(starts) != 1 {
		t.Fatalf("%d launches recorded, want 1", len(starts))
	}
	var launched struct {
		Args []string
		Env  []string
		Dir  string
	}
	b, _ := os.ReadFile(starts[0])
	if err := json.Unmarshal(b, &launched); err != nil {
		t.Fatal(err)
	}
	flag := func(name string) string {
		if i := slices.Index(launched.Args, name); i >= 0 && i+1 < len(launched.Args) {
			return launched.Args[i+1]
		}
		return ""
	}
	if flag("--resume") != "sess-person" || flag("--model") != "m-person" || flag("--config") != config || launched.Dir != g.dir {
		t.Fatalf("resumed with %q in %s", launched.Args, launched.Dir)
	}
	if !slices.ContainsFunc(launched.Env, func(kv string) bool { return strings.HasPrefix(kv, host.HookEnvVar+"=") }) {
		t.Fatalf("not under hataori's own host: %q", launched.Env)
	}

	// Removed: hataori's own of it goes, the person's directory stays.
	// The reply is posted while its turn still runs, and close refuses a
	// running session: close it once the turn has ended.
	eventually(t, "the resumed turn to end", r.within(15*time.Second), func() bool { return r.state(t, th) == "idle" })
	n := strconv.FormatInt(int64(id), 10)
	if code, _, e := cli(t, "close", n, "--root", r.root); code != 0 {
		t.Fatalf("hataori close: %s", e)
	}
	if code, _, e := cli(t, "remove", n, "--root", r.root); code != 0 {
		t.Fatalf("hataori remove: %s", e)
	}
	r.db(t).QueryRow(`SELECT state FROM threads WHERE id = ?`, int64(id)).Scan(&state)
	if state != "removed" {
		t.Fatalf("the thread is %s", state)
	}
	if _, err := os.Stat(runner.StateDir(r.root, sid)); !os.IsNotExist(err) {
		t.Fatalf("the session's state directory: %v", err)
	}
	if after := tree(t, g.dir); after != before {
		t.Fatalf("the person's directory changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The acceptance criterion of #401, the window going without a hook: the
// holder's Alive says gone, and the thread goes asleep with reason "window
// closed", its relay host ended.
func TestTakenInWindowKilled(t *testing.T) {
	r, _ := takeInRoot(t)
	g := newTakeRigWith(t, r.root, false, "")
	g.tmux(t, "new-window", "-d", "-t", "work", "sleep 600")
	id, sid := takeIn(t, r, g)
	g.tmux(t, "kill-pane", "-t", g.pane) // no hook runs
	closedWindow(t, r, id, sid)
}
