package chat

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// starts records what the core asked the runner to start.
type starts struct {
	mu    sync.Mutex
	l     []core.Launch
	calls []string // deliver TEXT, answer REQUEST TEXT
}

func (r *starts) record(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func (r *starts) handed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *starts) Start(_ context.Context, l core.Launch) error {
	r.mu.Lock()
	r.l = append(r.l, l)
	r.mu.Unlock()
	return nil
}
func (r *starts) Deliver(_ context.Context, _, _ int64, in control.Input) error {
	r.record("deliver " + in.Text)
	return nil
}
func (r *starts) Answer(_ context.Context, _, _ int64, req string, a control.Answer) error {
	r.record("answer " + req + " " + a.Text)
	return nil
}
func (r *starts) Interrupt(context.Context, int64) error { return nil }
func (r *starts) Resume(context.Context, int64, control.Principal, string, string, control.Input) error {
	return nil
}
func (r *starts) all() []core.Launch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Launch(nil), r.l...)
}

type workspaces struct{ err error }

func (workspaces) Adopt(context.Context, control.ThreadID, core.Placement) (string, bool, error) {
	return "", false, nil
}

func (workspaces) State(context.Context, string) (control.WorkspaceState, error) {
	return control.WorkspaceState{}, nil
}

func (w workspaces) Make(context.Context, control.ThreadID, string, func(core.Placement) error) (string, error) {
	return "dir", w.err
}

// logBuf is a log that tests read while the code under test writes it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := strings.TrimSpace(l.b.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

type front struct {
	fe     *FrontEnd
	driver *fake.Driver
	runner *starts
	log    *logBuf
}

const (
	owner   = "owner-1"
	hook    = "hook-1"
	channel = "c1"
)

var chanConv = surface.Conversation{Channel: channel}

// testPeople has the owner, and the hook as one of the channel's people.
func testPeople() *people.Directory {
	return people.New([]people.Person{{Name: "owner", IDs: []string{"chat:" + owner}}, {Name: "hook", IDs: []string{"chat:" + hook}}},
		"owner", map[string][]string{channel: {"hook"}})
}

func newFront(t *testing.T, ws workspaces, opts ...func(*FrontEnd)) *front {
	t.Helper()
	st := openStore(t, t.TempDir())
	t.Cleanup(func() { st.Close() })
	lb := &logBuf{}
	lg := slog.New(slog.NewTextHandler(lb, nil))
	d := fake.New()
	r := &starts{}
	fe := &FrontEnd{Name: "chat", Driver: d, Trigger: "!go", People: testPeople(), Log: lg,
		Outbox: &Outbox{Store: st, Client: "chat", Driver: d, Log: lg, Retry: 5 * time.Millisecond},
		Scopes: map[string]Scope{channel: {Enabled: true}, "off": {}}}
	c := &core.Core{Store: st, Machine: &session.Machine{Store: st}, Log: lg, Agent: "agent", Runner: r, Workspaces: ws,
		People: testPeople(),
		Scopes: map[string]core.Scope{channel: {Enabled: true, Owner: "chat"}, "off": {Owner: "chat"}},
		Owners: map[string]control.ScopeOwner{"chat": fe}}
	fe.API = c.As
	for _, o := range opts {
		o(fe)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fe.Outbox.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &front{fe: fe, driver: d, runner: r, log: lb}
}

// deliver receives a message on the fake surface and hands it to the
// front end, as the driver's Run would.
func (f *front) deliver(conv surface.Conversation, author surface.Author, text string) surface.Inbound {
	in := f.driver.Inject(conv, author, text)
	f.fe.Message(in)
	return in
}

// threads returns the threads opened in the channel.
func (f *front) threads() []fake.Call {
	var out []fake.Call
	for _, c := range f.driver.Calls(chanConv) {
		if c.Op == fake.OpOpenThread {
			out = append(out, c)
		}
	}
	return out
}

func TestTriggerStartsAThread(t *testing.T) {
	f := newFront(t, workspaces{})
	prompt := "Fix the login bug\n\nDetails on a second line."
	in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git "+prompt)
	ths := f.threads()
	if len(ths) != 1 || ths[0].Title != "Fix the login bug" || ths[0].From != in.Ref.ID {
		t.Fatalf("opened %+v, want one thread from the trigger %s, titled from the first line", ths, in.Ref.ID)
	}
	l := f.runner.all()
	if len(l) != 1 || l[0].Key != in.Ref.ID || l[0].Input.Text != prompt || l[0].Thread.Key != ths[0].Opened.Thread ||
		l[0].Principal != "hook" {
		t.Fatalf("started %+v", l)
	}
	waitFor(t, "the prompt in the thread", func() bool { return len(postsIn(f.driver, ths[0].Opened)) == 1 })
	if got := postsIn(f.driver, ths[0].Opened); got[0] != prompt {
		t.Fatalf("thread shows %q, want the full prompt", got)
	}
}

// A driver may deliver a message again, with the text as it is now; the
// session keeps the copy it started with.
func TestSameMessageTwiceStartsOneThreadAndEditsChangeNothing(t *testing.T) {
	f := newFront(t, workspaces{})
	in := f.deliver(chanConv, fake.Person(owner), "!go repo:https://example.com/r.git first")
	f.fe.Message(in)
	edited := in
	edited.Text = "!go repo:https://example.com/other.git edited"
	f.fe.Message(edited)
	if n := len(f.threads()); n != 1 {
		t.Fatalf("%d threads, want 1", n)
	}
	if l := f.runner.all(); len(l) != 1 || l[0].Input.Text != "first" {
		t.Fatalf("started %+v", l)
	}
	th := f.threads()[0].Opened
	// A row written after the prompt is posted after it, so once it is
	// there every prompt row is.
	if err := f.fe.Outbox.Write(context.Background(), row(1<<40, 0, th, "marker")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the marker", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), 1<<40, 0); return ok })
	if got := strings.Join(postsIn(f.driver, th), "|"); got != "first|marker" {
		t.Fatalf("thread shows %q", got)
	}
}

// A thread another client starts has no trigger message: it is opened in
// the channel alone.
func TestThreadOfAnotherClientOpensInTheChannel(t *testing.T) {
	f := newFront(t, workspaces{})
	api := f.fe.API(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpRead | control.OpStart})
	id, err := api.Start(context.Background(), control.StartRequest{Key: "k1", Scope: channel,
		Repo: "https://example.com/r.git", Input: control.Input{Text: "From elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	ths := f.threads()
	if len(ths) != 1 || ths[0].From != "" || ths[0].Opened.Thread != id.Key || ths[0].Title != "From elsewhere" {
		t.Fatalf("opened %+v for %+v, want one thread in the channel alone", ths, id)
	}
}

// failStart is the core for a front end whose Start fails, as when the
// daemon stops after the core asked for the thread and it was opened.
type failStart struct {
	control.API
	fe *FrontEnd
}

func (s failStart) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	if _, err := s.fe.NewThread(ctx, r.Scope, r.Key, r.Input.Text); err != nil {
		return control.ThreadID{}, err
	}
	return control.ThreadID{}, errors.New("stopped")
}

// The thread is opened once per trigger: delivered again after a restart,
// the trigger starts in the thread opened the first time.
func TestTriggerAfterARestartOpensNoSecondThread(t *testing.T) {
	f := newFront(t, workspaces{})
	api := f.fe.API
	fe := f.fe
	f.fe.API = func(c control.Caller) control.API { return failStart{api(c), fe} }
	in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
	if ths := f.threads(); len(ths) != 1 || len(f.runner.all()) != 0 {
		t.Fatalf("opened %+v, started %+v; want a thread and no start", ths, f.runner.all())
	}
	// A new front end on the same store, as after a restart.
	old := f.fe
	f.fe = &FrontEnd{Name: old.Name, Driver: old.Driver, Trigger: old.Trigger, People: old.People, Log: old.Log,
		Outbox: old.Outbox, Scopes: old.Scopes, API: api}
	f.fe.Message(in)
	f.fe.Message(in)
	ths := f.threads()
	if len(ths) != 1 {
		t.Fatalf("opened %+v, want one thread", ths)
	}
	if l := f.runner.all(); len(l) != 1 || l[0].Thread.Key != ths[0].Opened.Thread {
		t.Fatalf("started %+v, want one start in %s", l, ths[0].Opened.Thread)
	}
}

// The service opened the trigger's thread, but the answer was lost: the
// trigger delivered again opens no second thread and starts in that one.
func TestTriggerAfterALostThreadAnswerOpensOneThread(t *testing.T) {
	f := newFront(t, workspaces{})
	f.driver.FailAfter(fake.OpOpenThread, 1, nil)
	in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
	if l := f.runner.all(); len(l) != 0 {
		t.Fatalf("started %+v before the thread was known", l)
	}
	// Catch-up delivers it again: a message that failed is left to it.
	if err := f.fe.handle(in); err != nil {
		t.Fatalf("again: %v", err)
	}
	ths := f.threads()
	if len(ths) != 1 {
		t.Fatalf("opened %+v, want one thread", ths)
	}
	if l := f.runner.all(); len(l) != 1 || l[0].Thread.Key != ths[0].Opened.Thread {
		t.Fatalf("started %+v, want one start in %s", l, ths[0].Opened.Thread)
	}
}

func TestRefusedTriggersStartNothingAndLogOneLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		conv   surface.Conversation
		author surface.Author
		want   string
	}{
		// A guest: the core refuses the Start, and logs it with the id.
		{"author not allowed", chanConv, fake.Webhook("stranger"), `reason="a guest" client=chat id=stranger`},
		{"bot account", chanConv, fake.Bot(hook), "the author is a bot account"},
		{"scope not enabled", surface.Conversation{Channel: "off"}, fake.Person(owner), "the scope is not enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFront(t, workspaces{})
			before := len(f.log.lines())
			f.deliver(tc.conv, tc.author, "!go repo:https://example.com/r.git do it")
			if n := len(f.runner.all()); n != 0 {
				t.Fatalf("%d sessions started", n)
			}
			if c := f.driver.Calls(tc.conv); len(c) != 0 {
				t.Fatalf("calls in the channel: %+v", c)
			}
			lines := f.log.lines()[before:]
			if len(lines) != 1 || !strings.Contains(lines[0], tc.want) {
				t.Fatalf("log %q, want one line with %q", lines, tc.want)
			}
		})
	}
}

func TestLineBreakEndsTheRepo(t *testing.T) {
	f := newFront(t, workspaces{})
	f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git\nThe prompt")
	l := f.runner.all()
	if len(l) != 1 || l[0].Input.Text != "The prompt" {
		t.Fatalf("started %+v", l)
	}
	ths := f.threads()
	if ths[0].Title != "The prompt" {
		t.Fatalf("title %q", ths[0].Title)
	}
	var repo string
	f.fe.Outbox.Store.DB.QueryRow(`SELECT repo FROM threads`).Scan(&repo)
	if repo != "https://example.com/r.git" {
		t.Fatalf("repo %q", repo)
	}
}

func TestWorkspaceFailureIsShownOnce(t *testing.T) {
	f := newFront(t, workspaces{err: errors.New("clone failed: repository not found")})
	in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/missing.git do it")
	f.fe.Message(in)
	ths := f.threads()
	if len(ths) != 1 {
		t.Fatalf("%d threads", len(ths))
	}
	th := ths[0].Opened
	waitFor(t, "two posts", func() bool { return len(postsIn(f.driver, th)) >= 2 })
	if err := f.fe.Outbox.Write(context.Background(), row(1<<40, 0, th, "marker")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the marker", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), 1<<40, 0); return ok })
	got := postsIn(f.driver, th)
	if len(got) != 3 || got[0] != "do it" || !strings.Contains(got[1], "repository not found") {
		t.Fatalf("thread shows %q, want the prompt and one message with the reason", got)
	}
}

func TestParseTrigger(t *testing.T) {
	for _, tc := range []struct {
		text string
		ok   bool
		want order
	}{
		{"!go repo:u p", true, order{Repo: "u", Prompt: "p"}},
		{"  !go repo:u\r\nline one\nline two", true, order{Repo: "u", Prompt: "line one\nline two"}},
		{"!go p repo:u", true, order{Prompt: "p repo:u"}},
		{"!going repo:u p", false, order{}},
		{"say !go repo:u p", false, order{}},
		{"!go repo:\nu p", true, order{Prompt: "repo:\nu p"}},
	} {
		got, ok := parseTrigger("!go", tc.text)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseTrigger(%q) = %+v, %v; want %+v, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

// The daemon died after the core started the thread and before the
// front end wrote its rows; the driver delivers the message again, edited.
func TestRedeliveryAfterACrashShowsTheFirstText(t *testing.T) {
	for _, tc := range []struct {
		name string
		ws   workspaces
		want string
	}{
		{"started", workspaces{}, "first"},
		{"workspace failed", workspaces{err: errors.New("repository not found")}, "first|This thread could not start: repository not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFront(t, tc.ws)
			store := f.fe.Outbox.Store
			// Keep the sender from posting before the rows are taken away.
			store.DB.Exec(`CREATE TRIGGER lost AFTER INSERT ON chat_outbox BEGIN DELETE FROM chat_outbox WHERE id = NEW.id; END`)
			in := f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
			store.DB.Exec(`DROP TRIGGER lost`)
			// An edit that is no longer a trigger, then one that asks for more.
			in.Text = "the order was taken back"
			f.fe.Message(in)
			in.Text = "!go repo:https://example.com/r.git edited"
			f.fe.Message(in)
			th := f.threads()[0].Opened
			if err := f.fe.Outbox.Write(context.Background(), row(1<<40, 0, th, "marker")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the marker", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), 1<<40, 0); return ok })
			if got := strings.Join(postsIn(f.driver, th), "|"); got != tc.want+"|marker" {
				t.Fatalf("thread shows %q, want %q", got, tc.want)
			}
		})
	}
}
