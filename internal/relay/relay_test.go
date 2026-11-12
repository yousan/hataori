package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// The test binary is also the fake agent and its login command.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "role" && os.Args[2] == "fake" {
		os.Exit(fake.Main(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
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
	f.relay = &Relay{Store: st, Adapters: map[string]agent.Adapter{"fake": ad}, Owner: "owner", Timeout: timeout,
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
	f.seq++
	ctx := context.Background()
	if err := f.m.Apply(ctx, 1, session.Input{Kind: session.Send, Key: "m", Text: "go"}); err != nil {
		f.t.Fatal(err)
	}
	f.seq++
	if err := f.m.Apply(ctx, 1, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: f.seq, Kind: agent.TurnEnded, Status: status, Reason: reason}}); err != nil {
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := f.relay.Code("owner", "fake", code)
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
	fl := f.relay.flows["fake"]
	return fl != nil && syscall.Kill(-fl.cmd.Process.Pid, 0) == nil
}

func (f *fixture) pgid() int {
	f.relay.mu.Lock()
	defer f.relay.mu.Unlock()
	return f.relay.flows["fake"].cmd.Process.Pid
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
		if err := f.relay.Code("owner", "fake", c); !errors.Is(err, ErrNotACode) {
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
	if err := g.relay.Code("owner", "fake", goodCode); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("a code before the command asked: %v", err)
	}
	// Nor does an agent without a flow.
	if err := g.relay.Code("owner", "other", goodCode); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("a code for no flow: %v", err)
	}
}

func TestOnlyTheOwner(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	if err := f.relay.Start(context.Background(), "mallory", "fake"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("start by another: %v", err)
	}
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	if err := f.relay.Code("mallory", "fake", goodCode); !errors.Is(err, ErrForbidden) {
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
	if err := f.relay.Start(context.Background(), "owner", "fake"); err != nil {
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
		go func() { defer wg.Done(); f.relay.Start(context.Background(), "owner", "fake") }()
	}
	wg.Wait()
	ps, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	n := 0
	for _, p := range ps {
		if isLoginOf(p, f.root) {
			n++
		}
	}
	if n != 1 || !f.running() {
		t.Fatalf("%d login commands run; the tracked one runs: %v", n, f.running())
	}
}

// isLoginOf reports whether the process of the cmdline file p is a login
// command of this test's fake, by its command line.
func isLoginOf(p, root string) bool {
	b, _ := os.ReadFile(p)
	return bytes.Contains(b, []byte("login\x00--config\x00"+filepath.Join(root, "agent-config")))
}
