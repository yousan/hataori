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
	if err := r.Start(context.Background(), "chat:owner", "fake"); err != nil {
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
	pid := r.flows["fake"].cmd.Process.Pid
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
		err := f.relay.Code("chat:owner", "fake", code)
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
		if err := f.relay.Code("chat:owner", "fake", c); !errors.Is(err, ErrNotACode) {
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
	if err := g.relay.Code("chat:owner", "fake", goodCode); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("a code before the command asked: %v", err)
	}
	// Nor does an agent without a flow.
	if err := g.relay.Code("chat:owner", "other", goodCode); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("a code for no flow: %v", err)
	}
}

func TestOnlyTheOwner(t *testing.T) {
	f := newFixture(t, fake.Login{Code: goodCode}, 0)
	if err := f.relay.Start(context.Background(), "chat:mallory", "fake"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("start by another: %v", err)
	}
	f.turn(agent.Failed, agent.Login)
	f.next("the link")
	if err := f.relay.Code("chat:mallory", "fake", goodCode); !errors.Is(err, ErrForbidden) {
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
	if err := f.relay.Start(context.Background(), "chat:owner", "fake"); err != nil {
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
		go func() { defer wg.Done(); f.relay.Start(context.Background(), "chat:owner", "fake") }()
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
