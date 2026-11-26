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
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/runner"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/window"
	"github.com/yousan/hataori/internal/wire"
)

func init() {
	// The fake agent's hook in a person's window, as the Claude Code
	// plugin's is for Claude Code.
	windowHooks[fake.Name] = fake.WindowHookMain
}

// takeRig is a tmux server of the test's own with one pane, in which the
// fake agent runs as a person would start it: in a directory of theirs,
// reporting through `hataori hook --window`.
type takeRig struct {
	sock   []string // before every tmux command for this server
	pane   string   // the pane's id
	device string   // the pane's terminal
	pid    int      // the fake agent's process: the pane's command
	dir    string   // the person's directory
	window string   // the window, as ReportedSession.Window names it
	agent  string   // the command that started the agent in the pane
}

// personEnv is the environment of a person's own shell: the test's, without
// what would make the pane one of hataori's sessions, as when the test
// itself runs in one.
func personEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, host.HookEnvVar+"=") && !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_PANE=") {
			env = append(env, kv)
		}
	}
	return env
}

func (g *takeRig) tmux(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("tmux", append(append([]string{}, g.sock...), args...)...)
	cmd.Env = personEnv() // the server's, and so the pane's, environment
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newTakeRig(t *testing.T, root string) *takeRig { return newTakeRigIn(t, root, false) }

func newTakeRigIn(t *testing.T, root string, inShell bool) *takeRig {
	t.Helper()
	return newTakeRigWith(t, root, inShell, "")
}

// newTakeRigWith is newTakeRigIn, with extra arguments for the agent.
func newTakeRigWith(t *testing.T, root string, inShell bool, extra string) *takeRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no tmux")
		}
		t.Skip("no tmux")
	}
	g := &takeRig{sock: takeSocket()}
	// The cleanup comes first: only this test's own server, and its socket.
	t.Cleanup(func() {
		path := exec.Command("tmux", append(append([]string{}, g.sock...), "display-message", "-p", "#{socket_path}")...)
		path.Env = append(os.Environ(), "TMUX=")
		out, _ := path.Output()
		kill := exec.Command("tmux", append(append([]string{}, g.sock...), "kill-server")...)
		kill.Env = append(os.Environ(), "TMUX=")
		kill.Run()
		if p := strings.TrimSpace(string(out)); p != "" {
			os.Remove(p)
		}
	})
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.dir = dir
	// The person's agent is not hataori. Give the stand-in an independent
	// executable name so host-terminal ownership does not mistake it for
	// an attach command just because both use this test binary.
	agentBin := filepath.Join(t.TempDir(), "person-agent")
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentBin, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	agent := fmt.Sprintf("%s fake --terminal --window %s --session sess-person --model m-person --transcript %s --hook %s %s",
		q(agentBin), q(root), q(transcript), q(os.Args[0]), extra)
	g.agent = "exec " + agent
	if !inShell {
		g.tmux(t, "new-session", "-d", "-s", "work", "-x", "100", "-y", "30", "-c", dir, "exec "+agent)
	} else {
		// A shell of the person's, which starts the agent as a job of its
		// own, as a person types it.
		g.tmux(t, "new-session", "-d", "-s", "work", "-x", "100", "-y", "30", "-c", dir, "env -u ENV PS1='$ ' sh -i")
	}
	f := strings.Fields(g.tmux(t, "list-panes", "-a", "-F", "#{pane_id} #{pane_tty} #{pane_pid} #{socket_path},#{pid}"))
	if len(f) != 4 {
		t.Fatalf("panes: %q", f)
	}
	g.pane, g.device, g.window = f[0], f[1], "tmux:"+f[3]+","+f[0]
	g.pid, _ = strconv.Atoi(f[2])
	if inShell {
		g.tmux(t, "send-keys", "-t", g.pane, "-l", agent)
		g.tmux(t, "send-keys", "-t", g.pane, "Enter")
		// The pane's process is the shell, read once from the listing above:
		// asking tmux for it for every process listed made one look take
		// seconds, longer than the wait on a slow runner.
		if g.pid <= 0 {
			t.Fatalf("the shell's pid: %q", f[2])
		}
		shell := strconv.Itoa(g.pid)
		g.pid = 0
		for deadline := time.Now().Add(10 * time.Second); g.pid == 0; time.Sleep(10 * time.Millisecond) {
			out, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
			for _, l := range strings.Split(string(out), "\n") {
				if f := strings.Fields(l); len(f) == 2 && f[1] == shell {
					g.pid, _ = strconv.Atoi(f[0])
				}
			}
			if g.pid == 0 && time.Now().After(deadline) {
				procs, _ := exec.Command("ps", "-t", strings.TrimPrefix(g.device, "/dev/"), "-o", "pid=,ppid=,stat=,command=").Output()
				screen, _ := exec.Command("tmux", append(append([]string{}, g.sock...), "capture-pane", "-p", "-t", g.pane)...).CombinedOutput()
				t.Fatalf("timed out waiting for the agent's process, a child of the shell %s:\n%s\nscreen:\n%s", shell, procs, screen)
			}
		}
	}
	return g
}

// shellPID is the pane's command: the person's shell, for a rig in one.
func (g *takeRig) shellPID() int {
	out, _ := exec.Command("tmux", append(append([]string{}, g.sock...), "display-message", "-p", "-t", g.pane, "#{pane_pid}")...).Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// takeSocket is what comes before every tmux command for the test's own
// server: known before the daemon starts, which lists that server's panes.
func takeSocket() []string {
	return []string{"-L", fmt.Sprintf("hataori-take-test-%d", os.Getpid()), "-f", "/dev/null"}
}

// state is what a take-in and a release must leave as it is: the pane's
// command, size and options, and the processes on its terminal.
func (g *takeRig) state(t *testing.T) string {
	t.Helper()
	s := g.tmux(t, "display-message", "-p", "-t", g.pane, "#{pane_pid} #{pane_width}x#{pane_height} #{pane_dead}") + "\n" +
		g.tmux(t, "show-options", "-p", "-t", g.pane) + "\n" + g.tmux(t, "show-window-options", "-t", g.pane) + "\n"
	out, _ := exec.Command("ps", "-A", "-o", "pid=,tty=").Output()
	tty := strings.TrimPrefix(g.device, "/dev/")
	var pids []string
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == tty {
			pids = append(pids, f[0])
		}
	}
	slices.Sort(pids)
	return s + strings.Join(pids, " ")
}

// settled is the pane's state once no hook of the agent runs on its
// terminal: a hook is a process of its own there while it runs.
func (g *takeRig) settled(t *testing.T) string {
	t.Helper()
	var s string
	eventually(t, "the pane's hooks to end", 10*time.Second, func() bool {
		s = g.state(t)
		return strings.HasSuffix(s, "\n"+strconv.Itoa(g.pid))
	})
	return s
}

// The acceptance criteria of #400 with the fake agent in a tmux pane:
// `hataori take` says what answers the window's permission requests, then
// makes a thread with its first link whose session is the window's and
// whose workspace is not hataori's; a scope that is not configured and a
// window taken in already are refused; a message from chat reaches the
// session, whose final answer is posted once; a prompt typed in the window
// is posted marked as typed at the terminal; Stop sends Escape and the
// window's process runs on; `hataori release` puts the thread asleep,
// removes the window's record and the relay host, the window's hooks report
// again, and the pane is as it was.
func TestTakeInAWindowAndReleaseIt(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["host_terminals"] = true
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Terminal: true, Hataori: os.Args[0]}}
	})
	g := &takeRig{}
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = takeSocket() }
	t.Cleanup(func() { hostTermsHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	*g = *newTakeRig(t, r.root)
	before := g.settled(t)

	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A scope that is not configured: refused, nothing made.
	if code, _, _ := cli(t, "take", g.window, "--scope", "nowhere", "--root", r.root); code == 0 {
		t.Fatal("a take-in to a scope that is not configured was not refused")
	}

	// Taken in, once the window has reported its session.
	var out, errOut string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var code int
		if code, out, errOut = cli(t, "take", g.window, "--scope", "c1", "--key", "take-1", "--root", r.root); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not taken in: %s\nthe pane:\n%s", errOut, g.tmux(t, "capture-pane", "-p", "-t", g.pane))
		}
	}
	if !strings.Contains(errOut, `the rules of scope "c1" answer its permission requests`) {
		t.Fatalf("hataori take did not say what answers the window's permission requests: %q", errOut)
	}
	var took struct{ Thread control.ThreadID }
	if err := json.Unmarshal([]byte(out), &took); err != nil || took.Thread == 0 {
		t.Fatalf("hataori take printed %q", out)
	}
	th := r.thread(t, 0)
	if n := numberOf(t, r.root, th); n != strconv.FormatInt(int64(took.Thread), 10) {
		t.Fatalf("the first link is to thread %q, want %d", n, took.Thread)
	}
	db := r.db(t)
	var (
		sid                       int64
		session, workspace, state string
		foreign                   bool
	)
	eventually(t, "the session idle", 15*time.Second, func() bool {
		db.QueryRow(`SELECT s.id, s.agent_session_id, s.state, t.workspace, t.foreign_workspace FROM sessions s JOIN threads t ON t.id = s.thread_id
			WHERE t.id = ?`, int64(took.Thread)).Scan(&sid, &session, &state, &workspace, &foreign)
		return state == "idle"
	})
	if session != "sess-person" || workspace != g.dir || !foreign {
		t.Fatalf("session %q, workspace %q, not hataori's %v", session, workspace, foreign)
	}
	if code, _, _ := cli(t, "take", g.window, "--scope", "c1", "--key", "take-2", "--root", r.root); code == 0 {
		t.Fatal("a window taken in already was taken in again")
	}
	var threads int
	db.QueryRow(`SELECT count(*) FROM threads`).Scan(&threads)
	if threads != 1 {
		t.Fatalf("%d threads, want 1", threads)
	}

	// A message from chat reaches the session; its final answer is posted
	// once.
	owner := fakesurface.Person(e2eOwner)
	r.driver.Inject(th, owner, "reply from chat")
	r.posted(t, th, "from chat")
	eventually(t, "the turn's end", 15*time.Second, func() bool { return len(r.events(t, "turn_ended", th)) == 1 })
	time.Sleep(500 * time.Millisecond)
	if n := r.count(th, "from chat"); n != 1 {
		t.Fatalf("the final answer was posted %d times", n)
	}

	// A prompt typed in the window, by its person.
	g.tmux(t, "send-keys", "-t", g.pane, "-l", "reply typed here")
	g.tmux(t, "send-keys", "-t", g.pane, "Enter")
	r.posted(t, th, "(typed at the terminal)\ntyped here")

	// Stop interrupts with Escape; the window's process runs on, also past
	// the interrupt's grace.
	r.driver.Inject(th, owner, "tool wait 60s\nreply never")
	eventually(t, "the tool", 15*time.Second, func() bool { return len(r.events(t, "tool_started", th)) == 1 })
	if code, _, e := cli(t, "stop", strconv.FormatInt(int64(took.Thread), 10), "--root", r.root); code != 0 {
		t.Fatalf("hataori stop: %d %s", code, e)
	}
	eventually(t, "the interrupted turn's end", 15*time.Second, func() bool { return len(r.events(t, "turn_ended", th)) == 3 })
	if ended := r.events(t, "turn_ended", th); ended[2]["outcome"] == "completion" {
		t.Fatalf("the stopped turn: %v", ended[2])
	}
	time.Sleep(6 * time.Second)
	if !alive(g.pid) || g.tmux(t, "display-message", "-p", "-t", g.pane, "#{pane_dead}") != "0" {
		t.Fatal("stopping the turn ended the window's process")
	}

	// Released: asleep, its record and relay host gone, the pane as before.
	if code, _, e := cli(t, "release", strconv.FormatInt(int64(took.Thread), 10), "--root", r.root); code != 0 {
		t.Fatalf("hataori release: %d %s", code, e)
	}
	db.QueryRow(`SELECT state FROM sessions WHERE id = ?`, sid).Scan(&state)
	if state != "asleep" {
		t.Fatalf("released: the session is %s", state)
	}
	server := strings.TrimSuffix(strings.TrimPrefix(g.window, "tmux:"), ","+g.pane)
	if _, taken, err := window.ReadRecord(r.root, server, g.pane); taken || err != nil {
		t.Fatalf("the window's record after the release: %v, %v", taken, err)
	}
	dir := runner.StateDir(r.root, sid)
	eventually(t, "the relay host to end", 10*time.Second, func() bool { return host.Gone(dir) })
	if after := g.settled(t); after != before {
		t.Fatalf("the pane changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The window's hooks only report again: a turn typed there reaches no
	// thread, and the window is listed with what it reported.
	turns := len(r.events(t, "turn_started", th))
	g.tmux(t, "send-keys", "-t", g.pane, "-l", "reply after release")
	g.tmux(t, "send-keys", "-t", g.pane, "Enter")
	eventually(t, "the window's report after the release", 15*time.Second, func() bool {
		ts, err := c.HostTerminals(ctx)
		if err != nil {
			return false
		}
		for _, h := range ts {
			if h.Tmux != nil && h.Tmux.Pane == g.pane && h.Reported != nil && h.Reported.Session == "sess-person" && h.Reported.Event == "end" {
				return true
			}
		}
		return false
	})
	if n := len(r.events(t, "turn_started", th)); n != turns {
		t.Fatalf("a turn typed after the release reached the thread: %d turns, were %d", n, turns)
	}
}

// While the agent taken in is not the window's foreground process, as when
// it is suspended and its person's shell has the terminal back, input from
// chat goes nowhere: the shell would run it as a command.
func TestTakeInSendsNothingToTheShell(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["host_terminals"] = true
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Terminal: true, Hataori: os.Args[0]}}
	})
	g := &takeRig{}
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = takeSocket() }
	t.Cleanup(func() { hostTermsHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	*g = *newTakeRigIn(t, r.root, true)
	t.Cleanup(func() { syscall.Kill(g.pid, syscall.SIGCONT) })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		code, _, errOut := cli(t, "take", g.window, "--scope", "c1", "--key", "take-1", "--root", r.root)
		if code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not taken in: %s", errOut)
		}
	}
	th := r.thread(t, 0)
	eventually(t, "the session idle", 15*time.Second, func() bool {
		var state string
		r.db(t).QueryRow(`SELECT state FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&state)
		return state == "idle"
	})

	// Suspended: the shell takes the terminal back.
	syscall.Kill(g.pid, syscall.SIGSTOP)
	eventually(t, "the shell in the foreground", 10*time.Second, func() bool {
		out, _ := exec.Command("ps", "-o", "tpgid=", "-p", strconv.Itoa(g.shellPID())).Output()
		return strings.TrimSpace(string(out)) == strconv.Itoa(g.shellPID())
	})
	r.driver.Inject(th, fakesurface.Person(e2eOwner), "reply sent-to-the-shell")
	time.Sleep(3 * time.Second)
	if screen := g.tmux(t, "capture-pane", "-p", "-t", g.pane); strings.Contains(screen, "sent-to-the-shell") {
		t.Fatalf("chat input reached the person's shell:\n%s", screen)
	}
}
