package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// The test binary is also the fake agent and its login command.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "role" && os.Args[2] == "guard" {
		os.Exit(GuardMain(os.Args[3:]))
	}
	if len(os.Args) > 2 && os.Args[1] == "role" && os.Args[2] == "fake" {
		os.Exit(fake.Main(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 3 && os.Args[1] == "role" && os.Args[2] == "daemon" {
		os.Exit(daemonRole(os.Args[3]))
	}
	os.Exit(m.Run())
}

// daemonRole is a daemon a test kills: it starts a login flow under root
// and prints the login command's process id.
func daemonRole(root string) int {
	st, err := store.Open(context.Background(), root)
	if err != nil {
		return 1
	}
	cfg, _ := json.Marshal(fake.Config{Command: []string{os.Args[0], "role", "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")},
		ConfigDir: filepath.Join(root, "agent-config"), Login: fake.Login{Code: goodCode, Linger: true}})
	ad, _ := fake.New(cfg)
	link := make(chan struct{}, 1)
	r := &Relay{Store: st, Adapters: map[string]agent.Adapter{"fake": ad}, People: people.New([]people.Person{{Name: "owner", IDs: []string{"chat:owner"}}}, "owner", nil),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(u Update) {
			if u.Link != "" {
				select {
				case link <- struct{}{}:
				default:
				}
			}
		}, Guard: []string{os.Args[0], "role", "guard"}}
	if err := r.Start(context.Background(), "chat:owner", "fake", 0); err != nil {
		return 1
	}
	select {
	case <-link:
	case <-time.After(10 * time.Second):
		return 1
	}
	// This makes the losing order usual; correctness does not depend on it.
	time.Sleep(200 * time.Millisecond)
	r.mu.Lock()
	pid := r.flows[session.Login{Agent: "fake"}].cmd.Process.Pid
	r.mu.Unlock()
	os.Stdout.WriteString(itoa(pid) + "\n")
	select {}
}

const goodCode = "k7Q-unique-code-93x"

type fixture struct {
	t       *testing.T
	root    string
	st      *store.Store
	m       *session.Machine
	relay   *Relay
	updates chan Update
	seq     uint64
	logPath string
	logFile *os.File
	mu      sync.Mutex
	pids    []int
}

func newFixture(t *testing.T, l fake.Login, timeout time.Duration) *fixture {
	t.Helper()
	return newFixtureWith(t, l, timeout, nil)
}

// newFixtureWith is newFixture, with set applied to the relay before it runs.
func newFixtureWith(t *testing.T, l fake.Login, timeout time.Duration, set func(*Relay)) *fixture {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(fake.Config{Command: []string{os.Args[0], "role", "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")},
		ConfigDir: filepath.Join(root, "agent-config"), Login: l})
	os.Mkdir(filepath.Join(root, "agent-config"), 0o700)
	ad, err := fake.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "state", "daemon.log")
	lf, _ := os.Create(logPath)
	f := &fixture{t: t, root: root, st: st, m: &session.Machine{Store: st}, updates: make(chan Update, 32), logPath: logPath, logFile: lf}
	f.relay = &Relay{Store: st, Adapters: map[string]agent.Adapter{"fake": ad}, People: people.New([]people.Person{{Name: "owner", IDs: []string{"chat:owner"}}}, "owner", nil), Timeout: timeout,
		Log: slog.New(slog.NewTextHandler(lf, nil)), Notify: func(u Update) { f.updates <- u }}
	if _, err := st.DB.Exec(`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 'k', 'p', 'idle', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'fake', 'idle', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if set != nil {
		set(f.relay)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.relay.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; lf.Close(); st.Close() })
	time.Sleep(50 * time.Millisecond) // Run has read the head
	return f
}

// turn runs one turn of the thread that ends as given.
func (f *fixture) turn(status agent.EndStatus, reason agent.Reason) {
	f.t.Helper()
	f.turnOf(1, status, reason)
}

// turnOf is turn of session id.
func (f *fixture) turnOf(id int64, status agent.EndStatus, reason agent.Reason) {
	f.t.Helper()
	f.seq++
	ctx := context.Background()
	if err := f.m.Apply(ctx, id, session.Input{Kind: session.Send, Key: "m", Text: "go"}); err != nil {
		f.t.Fatal(err)
	}
	f.seq++
	if err := f.m.Apply(ctx, id, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: f.seq, Kind: agent.TurnEnded, Status: status, Reason: reason}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) next(what string) Update {
	f.t.Helper()
	select {
	case u := <-f.updates:
		return u
	case <-time.After(15 * time.Second):
		f.t.Fatalf("no update: waiting for %s", what)
	}
	return Update{}
}

// code hands code in as soon as the command asks for one.
func (f *fixture) code(code string) {
	f.t.Helper()
	f.codeFor(0, code)
}

// codeFor is code for the login of an outage.
func (f *fixture) codeFor(outage int64, code string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := f.relay.Code(context.Background(), "chat:owner", "fake", outage, code)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrNotWaiting) || time.Now().After(deadline) {
			f.t.Fatalf("code: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) loginEvents(kind control.EventKind) int {
	var n int
	f.st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, string(kind)).Scan(&n)
	return n
}

// running reports whether the flow's command (its process group) is alive.
func (f *fixture) running() bool {
	f.relay.mu.Lock()
	defer f.relay.mu.Unlock()
	fl := f.relay.flows[session.Login{Agent: "fake"}]
	return fl != nil && syscall.Kill(-fl.cmd.Process.Pid, 0) == nil
}

func (f *fixture) flowCount() int {
	f.relay.mu.Lock()
	defer f.relay.mu.Unlock()
	return len(f.relay.flows)
}

func (f *fixture) pgid() int {
	f.relay.mu.Lock()
	defer f.relay.mu.Unlock()
	return f.relay.flows[session.Login{Agent: "fake"}].cmd.Process.Pid
}

func TestLoginThroughTheRelay(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	f.turn(agent.Failed, agent.Login)
	u := f.next("the link")
	link := u.Link
	if link == "" || u.Expires.IsZero() {
		t.Fatalf("first update: %+v", u)
	}
	f.code(goodCode)
	if u := f.next("the result"); u.Done != LoggedIn {
		t.Fatalf("result: %+v", u)
	}
	var open int
	f.st.DB.QueryRow(`SELECT count(*) FROM login_outages WHERE closed_at IS NULL`).Scan(&open)
	if open != 0 || f.loginEvents(session.KindLoginRestored) != 1 {
		t.Fatalf("open outages %d, login_restored events %d", open, f.loginEvents(session.KindLoginRestored))
	}

	// The link and the code are nowhere under the root: not in the
	// database (its tables and events, and its journal), the log or any
	// other file.
	f.logFile.Sync()
	f.st.DB.Exec(`PRAGMA wal_checkpoint(FULL)`)
	filepath.Walk(f.root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, secret := range []string{link, goodCode, strings.TrimPrefix(link, "https://")} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s holds %q", p, secret)
			}
		}
		return nil
	})
	// Events, as the socket and `hataori events` serve them.
	evs, _ := f.st.Events(context.Background(), control.Filter{}, 0, 1000)
	for _, e := range evs {
		if bytes.Contains(e.Data, []byte(goodCode)) || bytes.Contains(e.Data, []byte(link)) {
			t.Errorf("event %s holds the link or the code", e.Kind)
		}
	}
	log, _ := os.ReadFile(f.logPath)
	if n := strings.Count(string(log), "login code arrived"); n != 1 || !strings.Contains(string(log), "length="+itoa(len(goodCode))) {
		t.Fatalf("log:\n%s", log)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRefusedCodeAndWaitingGoesOn(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	f.code("wrong-code")
	if u := f.next("the refusal"); u.Refused != "That code is not valid. Try again." {
		t.Fatalf("refusal: %+v", u)
	}
	f.code(goodCode)
	if u := f.next("the result"); u.Done != LoggedIn {
		t.Fatalf("result: %+v", u)
	}
}

func TestCodeOnlyWhenAskedAndOneWord(t *testing.T) {
	// Quit ends the command at the first line it reads: a write would end it.
	f := newFixture(t, fake.Login{Code: goodCode, Quit: true}, 0)
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	for _, c := range []string{"two words", "", "tab\there", "line\nbreak"} {
		if err := f.relay.Code(context.Background(), "chat:owner", "fake", 0, c); !errors.Is(err, ErrNotACode) {
			t.Errorf("code %q: %v", c, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if !f.running() {
		t.Fatal("a refused code reached the command")
	}

	// A command that never asks takes no code.
	g := newFixture(t, fake.Login{Code: goodCode, NoLink: true}, 0)
	g.turn(agent.Failed, agent.Login)
	time.Sleep(200 * time.Millisecond)
	if err := g.relay.Code(context.Background(), "chat:owner", "fake", 0, goodCode); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("a code before the command asked: %v", err)
	}
	// Nor does an agent without a flow.
	if err := g.relay.Code(context.Background(), "chat:owner", "other", 0, goodCode); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("a code for no flow: %v", err)
	}
}

func TestOnlyTheOwner(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	if err := f.relay.Start(context.Background(), "chat:mallory", "fake", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("start by another: %v", err)
	}
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	if err := f.relay.Code(context.Background(), "chat:mallory", "fake", 0, goodCode); !errors.Is(err, ErrForbidden) {
		t.Fatalf("code by another: %v", err)
	}
}

func TestTimeLimit(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 400*time.Millisecond)
	f.turn(agent.Failed, agent.Login)
	first := f.next("the link").Link
	pgid := f.pgid()
	if u := f.next("the end"); u.Done != Expired {
		t.Fatalf("after the time limit: %+v", u)
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a process of the login command remains: %v", err)
	}
	if err := f.relay.Start(context.Background(), "chat:owner", "fake", 0); err != nil {
		t.Fatal(err)
	}
	if again := f.next("a new link").Link; again == "" || again == first {
		t.Fatalf("links %q, %q", first, again)
	}
}

func TestCleanEndWithoutLogin(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode, Quit: true}, 0)
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	f.code(goodCode)
	if u := f.next("the result"); u.Done != Failed || u.Detail == "" {
		t.Fatalf("result: %+v", u)
	}
	if f.loginEvents(session.KindLoginRestored) != 0 {
		t.Fatal("the outage closed")
	}
}

func TestOutageClosedElsewhere(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	pgid := f.pgid()
	f.turn(agent.Completed, "") // someone logged in at the host
	if u := f.next("the end"); u.Done != Restored {
		t.Fatalf("result: %+v", u)
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a process of the login command remains: %v", err)
	}
}

func TestRelaySwitchedOff(t *testing.T) {
	for name, set := range map[string]func(*Relay){
		"login_relay false":   func(r *Relay) { r.Off = true },
		"no client to notify": func(r *Relay) { r.Notify = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixtureWith(t, fake.Login{Code: goodCode}, 0, set)
			f.turn(agent.Failed, agent.Login)
			time.Sleep(300 * time.Millisecond)
			if f.loginEvents(session.KindLoginLost) != 1 || f.running() {
				t.Fatalf("login_lost events %d, a login command runs: %v", f.loginEvents(session.KindLoginLost), f.running())
			}
		})
	}
}

// Starts at the same time leave one command, and it is the one tracked.
func TestConcurrentStarts(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); f.relay.Start(context.Background(), "chat:owner", "fake", 0) }()
	}
	wg.Wait()
	n := 0
	for _, args := range commandLines(t) {
		if strings.Contains(args+" ", " login --config "+filepath.Join(f.root, "agent-config")+" ") {
			n++
		}
	}
	if n != 1 || !f.running() {
		t.Fatalf("%d login commands run; the tracked one runs: %v", n, f.running())
	}
}

// commandLines returns the command line of every process, its arguments
// joined by spaces. It asks ps, as macOS has no /proc.
func commandLines(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-ww", "-o", "args=").Output()
	if err != nil {
		t.Fatal("ps:", err)
	}
	return strings.Split(string(out), "\n")
}

// A daemon that dies takes its login command with it.
func TestDaemonDeathEndsTheLogin(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	os.Mkdir(filepath.Join(root, "agent-config"), 0o700)
	d := exec.Command(os.Args[0], "role", "daemon", root)
	out, _ := d.StdoutPipe()
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Process.Kill(); d.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the daemon did not start a login: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	if pid <= 1 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("no login command: %q", line)
	}
	d.Process.Signal(syscall.SIGKILL)
	d.Wait()
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(-pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(-pid, syscall.SIGKILL) // only the group this test made
			t.Fatal("the login command outlived its daemon")
		}
	}
}

// The guard kills the login group when the daemon pipe closes.
func TestGuardDaemonDeath(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	cmd := exec.Command(os.Args[0], "role", "guard", "sh", "-c", "echo ready; exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{read}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	read.Close()
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	write.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guard did not exit")
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(-cmd.Process.Pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("login group outlived its daemon")
		}
	}
}

// The guard preserves the child's exit code.
func TestGuardExitCode(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(os.Args[0], "role", "guard", "sh", "-c", "exit 23")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{read}
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("guard exit: %v, want exit code 23", err)
	}
}

// A guard outside its own group refuses to start its child.
func TestGuardRequiresOwnGroup(t *testing.T) {
	cmd := exec.Command(os.Args[0], "role", "guard", "sh", "-c", "echo started")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || out.Len() != 0 {
		t.Fatalf("guard exit: %v, output: %q; want exit 1 and no child output", err, out.String())
	}
}

func TestDeviceLogin(t *testing.T) {
	for _, fail := range []string{"", "Error logging in: device auth timed out after 15 minutes"} {
		t.Run(fail, func(t *testing.T) {
			f := newFixture(t, fake.Login{Device: true, Code: "ABCD-12345", DeviceFail: fail}, 5*time.Second)
			f.turn(agent.Failed, agent.Login)
			first := f.next("link")
			if first.Link == "" || !first.NoCode || first.ShowCode != "" {
				t.Fatalf("first: %+v", first)
			}
			if err := f.relay.Code(context.Background(), "chat:owner", "fake", 0, "anything"); !errors.Is(err, ErrNotWaiting) {
				t.Fatalf("Code: %v", err)
			}
			second := f.next("code")
			if second.Link != first.Link || second.ShowCode != "ABCD-12345" || !second.NoCode {
				t.Fatalf("second: %+v", second)
			}
			end := f.next("end")
			if fail == "" {
				if end.Done != LoggedIn {
					t.Fatalf("end: %+v", end)
				}
				if f.loginEvents(session.KindLoginRestored) != 1 {
					t.Fatal("outage not closed")
				}
			} else if end.Done != Failed || end.Detail != fail || end.Refused != "" {
				t.Fatalf("end: %+v", end)
			}
		})
	}
}

// addSession adds a thread with an idle session of the agent that has a
// configuration directory of its own, as one taken in has.
func (f *fixture) addSession(id int64, dir string) {
	f.t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	spec, _ := json.Marshal(map[string]any{"agent": "fake", "spec": agent.Spec{ConfigDir: dir}})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (?, 's', ?, 'p', 'idle', 0, 0)`, []any{id, "k" + itoa(int(id))}},
		{`INSERT INTO sessions (id, thread_id, agent, state, spec, created_at, updated_at) VALUES (?, ?, 'fake', 'idle', ?, 0, 0)`, []any{id, id, string(spec)}},
	} {
		if _, err := f.st.DB.Exec(q.sql, q.args...); err != nil {
			f.t.Fatal(err)
		}
	}
}

// openOutages returns the configuration directories whose login is out.
func (f *fixture) openOutages() []string {
	f.t.Helper()
	rows, err := f.st.DB.Query(`SELECT config_dir FROM login_outages WHERE closed_at IS NULL ORDER BY id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

// The relay logs in where the session is logged out: in the configuration
// directory of a session taken in, and nowhere else.
func TestLoginInTheConfigDirOfTheSession(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	own, work := filepath.Join(f.root, "agent-config"), filepath.Join(f.root, "work")
	f.addSession(2, work)
	in := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, fake.LoggedInFile))
		return err == nil
	}
	f.turnOf(2, agent.Failed, agent.Login)
	w := f.next("the link of the one taken in")
	if w.Link == "" || w.Outage == 0 {
		t.Fatalf("first update: %+v", w)
	}
	f.turn(agent.Failed, agent.Login)
	o := f.next("the link of the other")
	if o.Link == "" || o.Outage == 0 || o.Outage == w.Outage {
		t.Fatalf("second update: %+v, after %+v", o, w)
	}
	// Both commands run: the second start ended no flow of another login.
	if n := f.flowCount(); n != 2 {
		t.Fatalf("%d flows run, want 2", n)
	}
	f.codeFor(w.Outage, goodCode)
	if u := f.next("the first result"); u.Done != LoggedIn || u.Outage != w.Outage {
		t.Fatalf("result: %+v", u)
	}
	if !in(work) || in(own) {
		t.Fatalf("logged in: in the session's directory %v, in the daemon's %v", in(work), in(own))
	}
	if got := f.openOutages(); len(got) != 1 || got[0] != "" {
		t.Fatalf("open outages of %q, want the daemon's only", got)
	}
	// The other goes on, and a new link for it is in its directory.
	if err := f.relay.Start(context.Background(), "chat:owner", "fake", o.Outage); err != nil {
		t.Fatal(err)
	}
	if u := f.next("the end of the replaced one"); u.Done != Stopped || u.Outage != o.Outage {
		t.Fatalf("replaced: %+v", u)
	}
	if u := f.next("the new link"); u.Link == "" || u.Outage != o.Outage {
		t.Fatalf("new link: %+v", u)
	}
	f.codeFor(o.Outage, goodCode)
	if u := f.next("the second result"); u.Done != LoggedIn || u.Outage != o.Outage {
		t.Fatalf("result: %+v", u)
	}
	if !in(own) || len(f.openOutages()) != 0 {
		t.Fatalf("logged in in the daemon's directory %v, open outages %q", in(own), f.openOutages())
	}
}

// A flow asked for with no outage is for the login's open one; one asked for
// an outage that has closed does not start, since nothing would show its link
// or end it.
func TestStartForAnOutageThatClosed(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	f.turn(agent.Failed, agent.Login)
	first := f.next("the link")
	// A button from before outages had a login of their own names none.
	if err := f.relay.Start(context.Background(), "chat:owner", "fake", 0); err != nil {
		t.Fatal(err)
	}
	if u := f.next("the end of the replaced one"); u.Done != Stopped {
		t.Fatalf("replaced: %+v", u)
	}
	if u := f.next("the new link"); u.Link == "" || u.Outage != first.Outage {
		t.Fatalf("new link %+v, want one for outage %d", u, first.Outage)
	}
	f.turn(agent.Completed, "") // someone logged in at the host
	if u := f.next("the end"); u.Done != Restored {
		t.Fatalf("result: %+v", u)
	}
	if err := f.relay.Start(context.Background(), "chat:owner", "fake", first.Outage); !errors.Is(err, ErrBack) {
		t.Fatalf("start for an outage that closed: %v, want %v", err, ErrBack)
	}
	if n := f.flowCount(); n != 0 {
		t.Fatalf("%d flows run, want none", n)
	}
	// Nor does it replace the flow of a later outage.
	f.turn(agent.Failed, agent.Login)
	if u := f.next("the link of the later outage"); u.Link == "" || u.Outage == first.Outage {
		t.Fatalf("link %+v", u)
	}
	if err := f.relay.Start(context.Background(), "chat:owner", "fake", first.Outage); !errors.Is(err, ErrBack) {
		t.Fatalf("start for the earlier outage: %v, want %v", err, ErrBack)
	}
	if n := f.flowCount(); n != 1 {
		t.Fatalf("%d flows run, want the later outage's", n)
	}
}

// A login that comes back by itself ends its own flow only.
func TestOutageClosedElsewhereOfOneDir(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	f.addSession(2, filepath.Join(f.root, "work"))
	f.turn(agent.Failed, agent.Login)
	o := f.next("the first link")
	f.turnOf(2, agent.Failed, agent.Login)
	w := f.next("the second link")
	f.turn(agent.Completed, "") // someone logged in at the host
	if u := f.next("the end"); u.Done != Restored || u.Outage != o.Outage {
		t.Fatalf("result: %+v, want the end of outage %d", u, o.Outage)
	}
	f.codeFor(w.Outage, goodCode)
	if u := f.next("the result"); u.Done != LoggedIn || u.Outage != w.Outage {
		t.Fatalf("result: %+v", u)
	}
}
