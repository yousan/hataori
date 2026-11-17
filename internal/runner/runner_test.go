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

type workDir string

func (w workDir) Make(context.Context, control.ThreadID, string) (string, error) {
	os.MkdirAll(string(w), 0o700)
	return string(w), nil
}

type options struct {
	terminal      bool
	instructions  string
	startTimeout  time.Duration
	idlePeriod    time.Duration
	conversations string // the fake's directory of conversations, if any
	contextBytes  int
	poll          time.Duration                     // 20ms when zero
	wrap          func(agent.Adapter) agent.Adapter // wraps the fake adapter, if set
}

// build makes an instance's store, core and runner with the fake agent.
func build(root string, o options, log io.Writer) (*store.Store, *core.Core, *Runner, error) {
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, _ := json.Marshal(fake.Config{Command: []string{os.Args[0], "role", "fake"}, Hataori: os.Args[0],
		Terminal: o.terminal, Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: o.conversations})
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
	c := &core.Core{Store: st, Machine: m, Scopes: map[string]core.Scope{"s": {Enabled: true, Owner: "chat"}},
		Workspaces: workDir(filepath.Join(root, "work")), Runner: r, Agent: "fake", Log: lg}
	return st, c, r, nil
}

const all = control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop

func owner(c *core.Core) control.API {
	return c.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
}

var thread = control.ThreadID{Scope: "s", Key: "t1"}

// killSessions kills every host and agent the runner started under root,
// and only those, and removes the socket directories that killed hosts
// left outside root.
func killSessions(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	for _, d := range dirs {
		for _, f := range []string{host.PIDFile, host.HostPIDFile} {
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
		if err := owner(f.core).Stop(context.Background(), thread); err != nil {
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
}
