package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/holder/tmux"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// heldMarker is a value of the agent's environment that must be on no
// command line.
const heldMarker = "marker-7c1e-not-on-a-command-line"

// heldServer is a tmux server of the test's own, standing for the default
// one, and removed when the test ends.
func heldServer(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no tmux")
		}
		t.Skip("no tmux")
	}
	server := []string{"-L", fmt.Sprintf("hataori-held-test-%d", os.Getpid()), "-f", "/dev/null"}
	t.Cleanup(func() {
		out, _ := tmuxOn(server, "display-message", "-p", "#{socket_path}").Output()
		tmuxOn(server, "kill-server").Run()
		if p := strings.TrimSpace(string(out)); p != "" {
			os.Remove(p)
		}
	})
	return server
}

func tmuxOn(server []string, args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", append(append([]string{}, server...), args...)...)
	cmd.Env = personEnv()
	return cmd
}

// heldRoot is an instance whose agents in a terminal run in tmux windows
// on server, or under hataori's own host when holder is "host": the fake
// agent with a start-up dialog to answer, a marker in its environment and
// a record of each of its starts.
func heldRoot(t *testing.T, holderName string, server []string) (*chatRoot, string) {
	t.Helper()
	// The daemon's environment, which the tmux server it starts inherits,
	// is that of a person's shell: not one of hataori's own sessions, as
	// when the test runs in one.
	t.Setenv(host.HookEnvVar, "")
	record := t.TempDir()
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["holder"] = holderName
		cfg["host_terminals"] = true
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH"), "HATAORI_TEST_MARKER=" + heldMarker}, Terminal: true, Hataori: os.Args[0],
			Dialog: "Do you trust the files in this folder? Enter to go on", Record: record}}
	})
	tmuxServerHook = func() []string { return server }
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = server }
	// Every tmux command the holder runs, short-lived ones included, goes
	// through a program that writes its arguments down first.
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrapper, args := filepath.Join(t.TempDir(), "tmux"), filepath.Join(t.TempDir(), "tmux-args")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+strconvQuote(args)+"\nexec "+strconvQuote(real)+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tmuxProgramHook = func() string { return wrapper }
	tmuxArgs = args
	t.Cleanup(func() { tmuxServerHook, hostTermsHook, tmuxProgramHook = nil, nil, nil })
	startDaemon(t, r.root)
	r.connected(t)
	return r, record
}

// tmuxArgs is the file the holder's tmux program writes its arguments to.
var tmuxArgs string

// strconvQuote quotes s for sh.
func strconvQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// startedHeld runs one thread to its first answer and returns its number and
// session.
func startedHeld(t *testing.T, r *chatRoot, n int, script, answer string) (surface.Conversation, control.ThreadID, int64) {
	t.Helper()
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" "+script)
	th := r.thread(t, n)
	r.posted(t, th, answer)
	num, _ := strconv.ParseInt(numberOf(t, r.root, th), 10, 64)
	var sid int64
	eventually(t, "the session idle", 15*time.Second, func() bool {
		var state string
		r.db(t).QueryRow(`SELECT id, state FROM sessions WHERE thread_id = ?`, num).Scan(&sid, &state)
		return state == "idle"
	})
	return th, control.ThreadID(num), sid
}

// launched is the environment the fake agent recorded at its one start,
// with the hooks' socket, which is the session's own, written as SOCKET.
func launched(t *testing.T, record, root string, sid int64) []string {
	t.Helper()
	starts, _ := filepath.Glob(filepath.Join(record, "start-*.json"))
	if len(starts) != 1 {
		t.Fatalf("%d starts recorded, want 1", len(starts))
	}
	b, _ := os.ReadFile(starts[0])
	var s struct{ Env []string }
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	own := host.HookEnv(runner.StateDir(root, sid))
	var env []string
	for _, kv := range s.Env {
		if strings.HasPrefix(kv, host.HookEnvVar+"=") {
			if kv != own {
				t.Fatalf("the hooks' socket %s, not the session's own %s", kv, own)
			}
			kv = host.HookEnvVar + "=SOCKET"
		}
		env = append(env, kv)
	}
	slices.Sort(env)
	return env
}

// heldWindow is the window the session in dir is held in.
func heldWindow(t *testing.T, dir string) holder.Window {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, runner.HeldFile))
	if err != nil {
		t.Fatal(err)
	}
	var w holder.Window
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// The acceptance criteria of #402 for a start, with "holder": "tmux":
//   - the relay host starts first, the agent in a window of a tmux session
//     of hataori's own name, whose log starts at the agent's first byte;
//     the start-up dialog is seen and answered through it, and the agent
//     answers;
//   - its environment is the one hataori's own host gives it, and no
//     value of it is on any process's command line;
//   - the dashboard's list shows the window once, as its thread.
func TestTmuxHolderStartsTheSession(t *testing.T) {
	server := heldServer(t)

	// The same start under hataori's own host, for its environment.
	own, ownRecord := heldRoot(t, "host", server)
	_, _, ownSid := startedHeld(t, own, 0, "reply under the own host", "under the own host")
	ownEnv := launched(t, ownRecord, own.root, ownSid)

	r, record := heldRoot(t, "tmux", server)
	_, id, sid := startedHeld(t, r, 0, "reply in tmux", "in tmux")
	dir := runner.StateDir(r.root, sid)

	// A tmux session of hataori's own name, its pane running the agent,
	// and the relay host beside it.
	sessions, _ := tmuxOn(server, "list-sessions", "-F", "#{session_name}").Output()
	if !strings.Contains(string(sessions), "hataori-") || !strings.HasSuffix(strings.TrimSpace(string(sessions)), "-"+strconv.FormatInt(sid, 10)) {
		t.Fatalf("tmux sessions: %q", sessions)
	}
	if spec, ok, _ := host.ReadSpec(dir); !ok || !spec.Relay || host.Gone(dir) {
		t.Fatalf("the relay host: %+v %v", spec, ok)
	}
	// The log from the agent's first byte: the dialog it shows at its start.
	if log := string(host.KeptTerminal(dir)); !strings.HasPrefix(log, "Do you trust the files") {
		t.Fatalf("the window's log starts %.80q", log)
	}

	// The environment hataori's own host gives, exactly.
	if env := launched(t, record, r.root, sid); !slices.Equal(env, ownEnv) {
		t.Fatalf("the agent's environment in tmux:\n%q\nunder the own host:\n%q", env, ownEnv)
	}
	// No value of it on any process's command line, while the agent runs.
	out, err := exec.Command("ps", "-A", "-ww", "-o", "args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), heldMarker) {
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, heldMarker) {
				t.Errorf("on a command line: %q", l)
			}
		}
		t.FailNow()
	}
	if !strings.Contains(string(out), " fake ") {
		t.Fatal("no process of the fake agent on the host: the search would find nothing anyway")
	}
	// Nor on the command line of any tmux command the holder ran, as
	// new-session, respawn-pane and pipe-pane, which end at once.
	ran, err := os.ReadFile(tmuxArgs)
	if err != nil || !strings.Contains(string(ran), "respawn-pane") {
		t.Fatalf("the tmux commands were not written down: %v", err)
	}
	if strings.Contains(string(ran), heldMarker) {
		t.Fatalf("a tmux command line holds a value of the environment:\n%s", ran)
	}

	// Listed once: as its thread, not as a terminal on the host.
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	d, err := c.Thread(ctx, id, 0, 0)
	if err != nil || d.Terminal == "" {
		t.Fatalf("the thread's terminal: %q, %v", d.Terminal, err)
	}
	pane, _ := tmuxOn(server, "list-panes", "-a", "-F", "#{pane_tty}").Output()
	if strings.TrimSpace(string(pane)) != d.Terminal {
		t.Fatalf("the thread's terminal is %s, the pane's %s", d.Terminal, pane)
	}
	ts, err := c.HostTerminals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(ts, func(h control.HostTerminal) bool { return h.Device == d.Terminal }) {
		t.Fatalf("the window is listed as a terminal on the host too: %+v", ts)
	}
}

// The acceptance criteria of #402 for an end, with "holder": "tmux":
//   - Stop leaves no process of the agent's group and removes the pane;
//   - the exit status is recorded from #{pane_dead_status};
//   - after the tmux server is killed the thread goes asleep with reason
//     interrupted, and a pane of a new server with the same id is never
//     taken for the old one.
func TestTmuxHolderEnds(t *testing.T) {
	server := heldServer(t)
	r, _ := heldRoot(t, "tmux", server)

	// Closed: the agent's group gone, the pane removed.
	_, id, sid := startedHeld(t, r, 0, "reply one", "one")
	dir := runner.StateDir(r.root, sid)
	w := heldWindow(t, dir)
	if code, _, e := cli(t, "close", strconv.FormatInt(int64(id), 10), "--root", r.root); code != 0 {
		t.Fatalf("hataori close: %s", e)
	}
	if !errorsIsESRCH(syscall.Kill(-w.Process, 0)) {
		t.Fatalf("a process of the agent's group %d is left", w.Process)
	}
	if out, _ := tmuxOn(server, "list-panes", "-a", "-F", "#{pane_id}").Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("panes left: %q", out)
	}
	if st := recordedStatus(t, dir); !st.Stopped {
		t.Fatalf("the stop's status: %+v", st)
	}

	// The agent exits by itself with status 3: recorded from the dead pane.
	th2, _, sid2 := startedHeld(t, r, 1, "reply two", "two")
	dir2 := runner.StateDir(r.root, sid2)
	w2 := heldWindow(t, dir2)
	r.driver.Inject(th2, fakesurface.Person(e2eOwner), "exit")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		b, err := os.ReadFile(filepath.Join(dir2, tmux.StatusFile))
		if err == nil && strings.Contains(string(b), `"code":3`) {
			break
		}
		if time.Now().After(deadline) {
			panes, _ := tmuxOn(server, "list-panes", "-a", "-F", "#{pane_id} #{pane_dead} #{pane_dead_status} #{pane_dead_signal} #{remain-on-exit} #{session_name}").CombinedOutput()
			_, held := os.Stat(filepath.Join(dir2, runner.HeldFile))
			t.Fatalf("the exit not recorded: %s %q %v; window %s, held.json %v; panes:\n%s", tmux.StatusFile, b, err, w2.Ref, held, panes)
		}
	}

	// The server killed: asleep, interrupted; a new server's pane with the
	// same id is not the old window.
	_, id3, sid3 := startedHeld(t, r, 2, "reply three", "three")
	dir3 := runner.StateDir(r.root, sid3)
	w3 := heldWindow(t, dir3)
	tmuxOn(server, "kill-server").Run()
	// The old server gone first, so that the new one is another.
	eventually(t, "the server gone", 10*time.Second, func() bool { return tmuxOn(server, "has-session").Run() != nil })
	if out, err := tmuxOn(server, "new-session", "-d", "-P", "-F", "#{pane_id}", "sleep 600").Output(); err != nil || strings.TrimSpace(string(out)) != w3pane(t, w3) {
		t.Fatalf("a new server's pane %q, want the old id %s: %v", out, w3pane(t, w3), err)
	}
	if l, err := (&tmux.Holder{}).Alive(context.Background(), w3); err != nil || l.State != holder.Gone {
		t.Fatalf("the old window on the new server: %+v, %v", l, err)
	}
	eventually(t, "asleep, interrupted", 20*time.Second, func() bool {
		var n int
		r.db(t).QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'state'
			AND json_extract(CAST(data AS TEXT), '$.to') = 'asleep' AND json_extract(CAST(data AS TEXT), '$.reason') = 'interrupted'`, int64(id3)).Scan(&n)
		return n == 1
	})
	// The window let go of on the old server's end, nothing on the new one
	// was touched: its pane of the same id is another's.
	eventually(t, "the window let go of", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir3, runner.HeldFile))
		return os.IsNotExist(err)
	})
	if out, err := tmuxOn(server, "list-panes", "-a", "-F", "#{pane_id}").Output(); err != nil || strings.TrimSpace(string(out)) != w3pane(t, w3) {
		t.Fatalf("the new server's pane after the old window was let go: %q %v", out, err)
	}
	// The killed server's agent did not outlive it.
	eventually(t, "the agent of the killed server gone", 10*time.Second, func() bool { return errorsIsESRCH(syscall.Kill(-w3.Process, 0)) })
}

// A Stop while a person has scrolled back in the agent's window: the window
// takes no key then, so the Stop says it was not taken, the turn goes on, and
// the agent is not ended as one that ignored an interrupt. Out of the copy
// mode, a Stop stops the turn (#741).
func TestTmuxHolderStopWhileScrolledBack(t *testing.T) {
	server := heldServer(t)
	r, _ := heldRoot(t, "tmux", server)
	th, id, sid := startedHeld(t, r, 0, "reply one", "one")
	w := heldWindow(t, runner.StateDir(r.root, sid))
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "tool wait 120s\nreply two")
	var turn int64
	eventually(t, "the turn running", 15*time.Second, func() bool {
		return r.db(t).QueryRow(`SELECT id FROM turns WHERE session_id = ? AND ended_at IS NULL`, sid).Scan(&turn) == nil
	})
	if out, err := tmuxOn(server, "copy-mode", "-t", w3pane(t, w)).CombinedOutput(); err != nil {
		t.Fatalf("copy-mode: %v: %s", err, out)
	}
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Stop(context.Background(), id, 0); err == nil || !strings.Contains(err.Error(), control.ErrNotInterrupted.Error()) {
		t.Fatalf("the Stop's answer: %v", err)
	}
	r.driver.InjectCommand(th, fakesurface.Person(e2eOwner), "stop")
	eventually(t, "the command's answer", 15*time.Second, func() bool {
		return posts(r.driver, th, fmt.Sprintf("Not stopped: the window of thread #%d's agent did not take the interrupt. The turn goes on.", id)) == 1
	})
	time.Sleep(7 * time.Second) // more than the grace after an interrupt
	if err := syscall.Kill(w.Process, 0); err != nil {
		t.Fatalf("the agent: %v", err)
	}
	var by, ended string
	if err := r.db(t).QueryRow(`SELECT stopped_by, coalesce(ended_at, '') FROM turns WHERE id = ?`, turn).Scan(&by, &ended); err != nil || by != "" || ended != "" {
		t.Fatalf("the turn after the refused stops: stopped by %q, ended %q, %v", by, ended, err)
	}

	if out, err := tmuxOn(server, "send-keys", "-X", "-t", w3pane(t, w), "cancel").CombinedOutput(); err != nil {
		t.Fatalf("leave copy mode: %v: %s", err, out)
	}
	if err := c.Stop(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn stopped", 15*time.Second, func() bool {
		var reason string
		r.db(t).QueryRow(`SELECT coalesce(reason, ''), stopped_by FROM turns WHERE id = ?`, turn).Scan(&reason, &by)
		return reason == "interrupted" && by == "owner"
	})
	if err := syscall.Kill(w.Process, 0); err != nil {
		t.Fatalf("the agent after the Stop: %v", err)
	}
}

func errorsIsESRCH(err error) bool { return err == syscall.ESRCH }

// w3pane is the pane id the window's reference names.
func w3pane(t *testing.T, w holder.Window) string {
	t.Helper()
	var r struct{ Pane string }
	if err := json.Unmarshal([]byte(w.Ref), &r); err != nil {
		t.Fatal(err)
	}
	return r.Pane
}

// recordedStatus is how the agent of the window in dir ended, as recorded.
func recordedStatus(t *testing.T, dir string) host.Status {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, tmux.StatusFile))
	if err != nil {
		t.Fatal(err)
	}
	var st host.Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}
