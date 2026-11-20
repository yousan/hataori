package runner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// The test binary is also `hataori host`, `hataori hook`, the fake agent and
// a daemon that a test can kill.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(host.HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 2 && os.Args[1] == "role" {
		switch os.Args[2] {
		case "host":
			if host.Run(os.Args[3]) != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "fake":
			os.Exit(fake.Main(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
		case "daemon":
			os.Exit(daemon(os.Args[3:]))
		}
		os.Exit(99)
	}
	os.Exit(m.Run())
}

// syncBuffer is a log both a test and the runner touch.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// workDir makes one directory per thread under root, as the workspace
// maker does: made aside, with a file in it, then renamed into place. With
// die, it kills its own process right after the rename.
type workDir struct {
	root string
	die  bool
}

func (w workDir) Make(_ context.Context, th control.ThreadID, _ string, placed func(core.Placement) error) (string, error) {
	target := filepath.Join(w.root, fmt.Sprint(int64(th)))
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("workspace exists: %s", target)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(w.root), "clone-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "notes.txt"), []byte("someone's work\n"), 0o600); err != nil {
		return "", err
	}
	st, err := os.Lstat(tmp)
	if err != nil {
		return "", err
	}
	sys := st.Sys().(*syscall.Stat_t)
	if err := placed(core.Placement{Dir: target, Dev: uint64(sys.Dev), Inode: sys.Ino}); err != nil {
		return "", err
	}
	os.MkdirAll(w.root, 0o700)
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	if w.die {
		syscall.Kill(os.Getpid(), syscall.SIGKILL) // this process only
		select {}
	}
	return target, nil
}

func (w workDir) Adopt(_ context.Context, th control.ThreadID, p core.Placement) (string, bool, error) {
	target := filepath.Join(w.root, fmt.Sprint(int64(th)))
	st, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sys := st.Sys().(*syscall.Stat_t)
	if p.Dir != target || uint64(sys.Dev) != p.Dev || sys.Ino != p.Inode {
		return "", false, fmt.Errorf("not the directory this thread's Make placed: %s", target)
	}
	return target, true, nil
}

func (workDir) State(context.Context, string) (control.WorkspaceState, error) {
	return control.WorkspaceState{}, nil
}

func (w workDir) Empty(_ context.Context, th control.ThreadID) (string, error) {
	dir := filepath.Join(w.root, "confined-"+fmt.Sprint(int64(th)))
	return dir, os.MkdirAll(dir, 0o700)
}

type options struct {
	terminal      bool
	instructions  string
	startTimeout  time.Duration
	idlePeriod    time.Duration
	conversations string // the fake's directory of conversations, if any
	contextBytes  int
	poll          time.Duration                     // 20ms when zero
	inputTimeout  string                            // the fake's; 5s when empty
	dieWhenPlaced bool                              // the workspace maker kills its process right after it placed a workspace
	wrap          func(agent.Adapter) agent.Adapter // wraps the fake adapter, if set
	dialog        string                            // the fake's start-up dialog, in the terminal form
	confines      bool                              // the fake takes confined specs, and the runner gives them no MCP server
}

// build makes an instance's store, core and runner with the fake agent.
func build(root string, o options, log io.Writer) (*store.Store, *core.Core, *Runner, error) {
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, _ := json.Marshal(fake.Config{Command: []string{os.Args[0], "role", "fake"}, Hataori: os.Args[0],
		Terminal: o.terminal, Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: o.conversations,
		InputTimeout: cmp.Or(o.inputTimeout, "5s"), Dialog: o.dialog, Confines: o.confines})
	ad, err := fake.New(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	var adapter agent.Adapter = ad
	if o.wrap != nil {
		adapter = o.wrap(adapter)
	}
	lg := slog.New(slog.NewTextHandler(log, nil))
	m := &session.Machine{Store: st}
	r := &Runner{Store: st, Machine: m, Adapters: map[string]agent.Adapter{"fake": adapter},
		Host: host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, Root: root, Log: lg,
		Instructions: o.instructions, StartTimeout: o.startTimeout, InterruptGrace: 5 * time.Second, Poll: cmp.Or(o.poll, 20*time.Millisecond),
		IdlePeriod: o.idlePeriod, ContextBytes: o.contextBytes}
	if o.confines {
		r.Confine = func(context.Context, ConfinedLaunch) (Confined, error) { return Confined{}, nil }
	}
	c := &core.Core{Store: st, Machine: m, People: people.New([]people.Person{{Name: "owner", IDs: []string{ownerFrom}}}, "owner", nil), Scopes: map[string]core.Scope{"s": {Enabled: true, Owner: "chat"}},
		Workspaces: workDir{root: filepath.Join(root, "work"), die: o.dieWhenPlaced}, Runner: r, Agent: "fake", Log: lg}
	return st, c, r, nil
}

const all = control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop

// ownerFrom is the owner's id: the tests' calls are relayed from it, as a
// chat front end's are.
const ownerFrom = "chat:1"

func owner(c *core.Core) control.API {
	return c.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all})
}

var thread = control.ThreadID(1)

// killSessions kills every host and agent the runner started under root,
// and only those, and removes the socket directories that killed hosts
// left outside root.
func killSessions(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	for _, d := range dirs {
		// The host first: killed after its command, it could see the
		// command end and record it in its status, which no reboot does.
		for _, f := range []string{host.HostPIDFile, host.PIDFile} {
			if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
					syscall.Kill(-pid, syscall.SIGKILL)
				}
			}
		}
		if sock := filepath.Dir(host.SocketPath(d)); strings.HasPrefix(filepath.Base(sock), "hataori-host-") {
			os.RemoveAll(sock)
		}
	}
}

type fixture struct {
	t    *testing.T
	root string
	st   *store.Store
	core *core.Core
	run  *Runner
	log  *syncBuffer
}

func newFixture(t *testing.T, o options) *fixture {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	log := &syncBuffer{}
	st, c, r, err := build(root, o, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); st.Close() })
	return &fixture{t: t, root: root, st: st, core: c, run: r, log: log}
}

// counts returns how many events of each kind the log holds.
func counts(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	rows, err := st.DB.Query(`SELECT kind, count(*) FROM events GROUP BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		m[k] = n
	}
	return m
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fixture) has(kind string, n int) func() bool {
	return func() bool { return counts(f.t, f.st)[kind] >= n }
}

func (f *fixture) start(script string) {
	f.t.Helper()
	if _, err := owner(f.core).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
		Repo: "file:///unused", Input: control.Input{Text: script}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) eventData(kind string) []map[string]any {
	rows, _ := f.st.DB.Query(`SELECT data FROM events WHERE kind = ? ORDER BY pos`, kind)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		var m map[string]any
		json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}

func forms(t *testing.T, f func(t *testing.T, terminal bool)) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) { f(t, terminal) })
	}
}

// The thread and its turn record the person and the id the input came from.
func TestRecordsKeepThePersonAndTheID(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	waitFor(t, "the end of the turn", 15*time.Second, f.has("turn_ended", 1))
	for _, kind := range []string{"thread_created", "turn_started", "turn_ended"} {
		d := f.eventData(kind)
		if len(d) != 1 || d[0]["principal"] != "owner" || d[0]["from"] != ownerFrom {
			t.Errorf("%s: %v", kind, d)
		}
	}
	var from string
	f.st.DB.QueryRow(`SELECT from_id FROM turns`).Scan(&from)
	if from != ownerFrom {
		t.Errorf("turns.from_id = %q", from)
	}
}

func TestATurnRunsToItsEnd(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		f := newFixture(t, options{terminal: terminal})
		f.start("reply hello")
		waitFor(t, "the end of the turn", 15*time.Second, f.has("turn_ended", 1))
		ends := f.eventData("turn_ended")
		texts := f.eventData("text")
		if ends[0]["outcome"] != "completion" || len(texts) != 1 || texts[0]["text"] != "hello" || texts[0]["final"] != true {
			t.Fatalf("end %v, texts %v", ends, texts)
		}
	})
}

func TestUnknownLineIsSkipped(t *testing.T) {
	f := newFixture(t, options{})
	f.start("unknown\nreply after")
	waitFor(t, "the end of the turn", 15*time.Second, f.has("turn_ended", 1))
	if !strings.Contains(f.log.String(), "skipped a line the codec does not know") {
		t.Fatalf("not logged: %s", f.log.String())
	}
}

func TestBlockedAtStart(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		f := newFixture(t, options{terminal: terminal, instructions: "never-start", startTimeout: time.Second})
		f.start("reply never")
		waitFor(t, "the failed start", 15*time.Second, func() bool {
			for _, d := range f.eventData("state") {
				if d["to"] == "failed" {
					return true
				}
			}
			return false
		})
		for _, d := range f.eventData("state") {
			if d["to"] == "failed" {
				if d["reason"] != string(agent.BlockedAtStart) || !strings.Contains(fmt.Sprint(d["detail"]), "never reporting the start") {
					t.Fatalf("failed state event %v", d)
				}
			}
		}
		// Its processes are stopped.
		dir := StateDir(f.root, 1)
		waitFor(t, "the host to end", 15*time.Second, func() bool { return host.Gone(dir) })
	})
}

// A start-up dialog no hook reports: the codec asked for the screen, the
// runner gave it until the codec stopped watching and carried out the key
// it sent. The fake writes to its screen after the answer and shows its
// dialog again after the start; its codec panics if the screen reaches it
// after it stopped watching or after the start.
// The screen goes to a codec that asked for it until the codec ends the
// watch, past the session's start: the fake watches on until its first
// turn ends, and panics if given the screen after that. The first turn
// writes to the screen, which it does only after the codec decoded the
// start (the input is pasted then), and waits on a question until the
// codec was given that line. The hook calls and the terminal
// are separate streams: a turn that ended at once could have its end read
// before its screen reached the log, and what the fake writes right after
// its ready report could be read before the report (#516).
func TestScreenUntilTheCodecStops(t *testing.T) {
	f := newFixture(t, options{terminal: true, dialog: "fake: trust this folder? Enter"})
	f.start("screen the turn's screen\nask go on?\nreply hello")
	waitFor(t, "the question", 15*time.Second, f.has("request", 1))
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn's screen", 15*time.Second, func() bool {
		lv.mu.Lock()
		defer lv.mu.Unlock()
		return bytes.Contains(fake.ScreenAfterStart(lv.codec), []byte("the turn's screen"))
	})
	var req string
	f.st.DB.QueryRow(`SELECT json_extract(data, '$.request') FROM events WHERE kind = 'request'`).Scan(&req)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := owner(f.core).Answer(ctx, control.RequestID(req), control.Answer{Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	// The question ends the first turn as blocked; the answer's turn is
	// the agent's first turn going on.
	waitFor(t, "the end of the answered turn", 15*time.Second, f.has("turn_ended", 2))
	if texts := f.eventData("text"); len(texts) == 0 || texts[len(texts)-1]["text"] != "hello" {
		t.Fatalf("texts %v", texts)
	}
	lv.mu.Lock()
	watching := lv.screen
	lv.mu.Unlock()
	if watching {
		t.Fatal("the runner still watches the screen after the codec ended the watch")
	}
}

// A codec rebuilt after a failed commit was not opened, so the runner
// stops giving the screen: the rebuilt fake never ends a watch it did not
// ask for, and panics if given the screen.
func TestNoScreenForARebuiltCodec(t *testing.T) {
	f := newFixture(t, options{terminal: true, dialog: "fake: trust this folder? Enter", poll: 200 * time.Millisecond})
	// The tool's start fails to commit; its end comes a second later, after
	// steps that read the screen.
	if _, err := f.st.DB.Exec(`CREATE TRIGGER fail_tool BEFORE INSERT ON events WHEN NEW.kind = 'tool_started' BEGIN SELECT RAISE(ABORT, 'test: no tool'); END`); err != nil {
		t.Fatal(err)
	}
	f.start("tool Bash 1s\nreply hello")
	waitFor(t, "a failed commit", 15*time.Second, func() bool { return strings.Contains(f.log.String(), "test: no tool") })
	if _, err := f.st.DB.Exec(`DROP TRIGGER fail_tool`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the end of the turn", 15*time.Second, f.has("turn_ended", 1))
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	lv.mu.Lock()
	watching := lv.screen
	lv.mu.Unlock()
	if watching {
		t.Fatal("the runner watches the screen for a codec that was not opened")
	}
}

func TestPartialLineWaitsForItsEnd(t *testing.T) {
	b := []byte("{\"t\":\"one\"}\n{\"t\":\"tw")
	end := complete(b, 0)
	if got := lines(b, 0, end); len(got) != 1 || end != int64(len("{\"t\":\"one\"}\n")) {
		t.Fatalf("lines %q, end %d", got, end)
	}
	b = append(b, []byte("o\"}\n")...)
	if got := lines(b, end, complete(b, end)); len(got) != 1 || string(got[0]) != `{"t":"two"}` {
		t.Fatalf("the rest: %q", got)
	}
}

// The same, through the runner: a line written in two halves to the log
// is decoded once its end arrives.
func TestHalfLineInTheLog(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply first")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	events := filepath.Join(StateDir(f.root, 1), host.EventsFile)
	lf, err := os.OpenFile(events, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	lf.WriteString(`{"t":"file","pa`)
	time.Sleep(200 * time.Millisecond) // several polls of the runner
	if counts(t, f.st)["file_out"] != 0 {
		t.Fatal("half a line was decoded")
	}
	lf.WriteString(`th":"/tmp/x"}` + "\n")
	waitFor(t, "the whole line", 5*time.Second, f.has("file_out", 1))
}

// groupMembers lists the processes of a process group, by ps.
func groupMembers(t *testing.T, pgid int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=").Output()
	if err != nil {
		t.Skipf("ps: %v", err)
	}
	var pids []int
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			pid, _ := strconv.Atoi(f[0])
			g, _ := strconv.Atoi(f[1])
			if g == pgid {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func TestStopEndsARunningTool(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		f := newFixture(t, options{terminal: terminal})
		f.start("tool long 60s\nreply late")
		waitFor(t, "the tool", 15*time.Second, f.has("tool_started", 1))
		pid, _ := strconv.Atoi(strings.TrimSpace(string(readFile(t, filepath.Join(StateDir(f.root, 1), host.PIDFile)))))
		if len(groupMembers(t, pid)) < 2 {
			t.Fatal("the tool's process is not in the session's group")
		}
		start := time.Now()
		if err := owner(f.core).Stop(context.Background(), thread, 0); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the end of the turn", 10*time.Second, f.has("turn_ended", 1))
		waitFor(t, "the tool's process to go", 10*time.Second-time.Since(start), func() bool {
			for _, p := range groupMembers(t, pid) {
				if p != pid {
					return false
				}
			}
			return true
		})
		if d := f.eventData("turn_ended"); d[0]["reason"] != session.ReasonInterrupted {
			t.Fatalf("turn end %v", d)
		}
	})
}

// A message sent right after a stop starts the next turn as soon as the
// interrupted one ends. The watcher of the interrupt waits for the turn it
// interrupted only: the next turn, longer than the grace period, runs to
// its end.
func TestNextTurnAfterAStopIsNotStopped(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		f := newFixture(t, options{terminal: terminal})
		f.start("tool long 60s\nreply late")
		waitFor(t, "the tool", 15*time.Second, f.has("tool_started", 1))
		// The watcher looks only once the next turn has started, so that it
		// never sees the session between the two turns.
		var once sync.Once
		f.run.point = func(p string) {
			if p == "interrupt watch" {
				once.Do(func() { waitFor(t, "the next turn", 15*time.Second, f.has("turn_started", 2)) })
			}
		}
		api := owner(f.core)
		if err := api.Stop(context.Background(), thread, 0); err != nil {
			t.Fatal(err)
		}
		if err := api.Send(context.Background(), thread, "k2", control.Input{Text: "tool wait 7s\nreply second"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the end of the next turn", 20*time.Second, f.has("turn_ended", 2))
		d := f.eventData("turn_ended")
		if d[0]["reason"] != session.ReasonInterrupted || d[1]["outcome"] != "completion" {
			t.Fatalf("turn ends %v; want the first interrupted and the next completed", d)
		}
		if strings.Contains(f.log.String(), "stopping the session") {
			t.Fatalf("the session was stopped: %s", f.log.String())
		}
	})
}

func readFile(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestNoAgentNameAndNoTerminalLog checks the runner's source.
func TestNoAgentNameAndNoTerminalLog(t *testing.T) {
	src := readFile(t, "runner.go")
	for _, bad := range []string{"claude", "Claude", "codex", "Codex", "fake", "TerminalLog", "terminal.log"} {
		if bytes.Contains(src, []byte(bad)) {
			t.Errorf("runner.go contains %q", bad)
		}
	}
	// terminal.go passes the terminal on to a person, and names no agent.
	src = readFile(t, "terminal.go")
	for _, bad := range []string{"claude", "Claude", "codex", "Codex", "fake"} {
		if bytes.Contains(src, []byte(bad)) {
			t.Errorf("terminal.go contains %q", bad)
		}
	}
}
