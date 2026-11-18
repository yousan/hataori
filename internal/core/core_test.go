package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *fakeRunner) record(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
	return nil
}
func (r *fakeRunner) Start(_ context.Context, l Launch) error {
	return r.record("start " + l.Input.Text)
}
func (r *fakeRunner) Deliver(_ context.Context, _, _ int64, in control.Input) error {
	return r.record("deliver " + in.Text)
}
func (r *fakeRunner) Answer(_ context.Context, _, _ int64, req string, _ control.Answer) error {
	return r.record("answer " + req)
}
func (r *fakeRunner) Interrupt(context.Context, int64) error { return r.record("interrupt") }
func (r *fakeRunner) Resume(_ context.Context, _ int64, _ control.Principal, _ string, in control.Input) error {
	return r.record("resume " + in.Text)
}
func (r *fakeRunner) Calls() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, "; ")
}

type fakeOwner struct {
	key string
	err error
}

func (o fakeOwner) NewThread(context.Context, string, string) (string, error) { return o.key, o.err }

type fakeWorkspaces struct{}

func (fakeWorkspaces) Make(context.Context, control.ThreadID, string, func(Placement) error) (string, error) {
	return "dir", nil
}

func (fakeWorkspaces) Adopt(context.Context, control.ThreadID, Placement) (string, bool, error) {
	return "", false, nil
}

// State reports one more uncommitted file than the length of dir, so a test
// sees which directory was asked about.
func (fakeWorkspaces) State(_ context.Context, dir string) (control.WorkspaceState, error) {
	return control.WorkspaceState{Branch: "main", Uncommitted: len(dir) + 1}, nil
}

const all = control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop

type fixture struct {
	core   *Core
	runner *fakeRunner
	log    *bytes.Buffer
	owner  control.API
}

func newFixture(t *testing.T, owner fakeOwner) *fixture {
	t.Helper()
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{runner: &fakeRunner{}, log: &bytes.Buffer{}}
	f.core = &Core{
		Store:   st,
		Machine: &session.Machine{Store: st},
		Scopes: map[string]Scope{
			"s":   {Enabled: true, Owner: "chat", Model: "m-default", Effort: "e-default"},
			"off": {Enabled: false, Owner: "chat"},
		},
		Owners:     map[string]control.ScopeOwner{"chat": owner},
		Workspaces: fakeWorkspaces{},
		Runner:     f.runner,
		Agent:      "fake",
		Log:        slog.New(slog.NewTextHandler(f.log, nil)),
	}
	f.owner = f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s", "off"}, May: all})
	return f
}

func (f *fixture) agentEvent(t *testing.T, sid int64, seq uint64, e agent.Event) {
	t.Helper()
	e.Seq = seq
	if err := f.core.Machine.Apply(context.Background(), sid, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
		t.Fatal(err)
	}
}

// running starts a thread and brings it to a running turn.
func (f *fixture) running(t *testing.T, key string) (control.ThreadID, int64) {
	t.Helper()
	ctx := context.Background()
	id, err := f.owner.Start(ctx, control.StartRequest{Key: key, Scope: "s", Thread: key, Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
	if err := f.owner.Send(ctx, id, key+"-first", control.Input{Text: "go"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	return id, sid
}

func count(t *testing.T, st *store.Store, table string) int {
	var n int
	if err := st.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEveryOperationChecksTheCaller(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1"}})

	callers := map[string]control.Caller{
		"other scope":  {Principal: "x", Scopes: []string{"elsewhere"}, May: all},
		"no operation": {Principal: "x", Scopes: []string{"s"}},
	}
	ops := map[string]func(control.API) error{
		"Threads": func(a control.API) error { _, err := a.Threads(ctx, control.Filter{Scope: "s"}); return err },
		"Thread":  func(a control.API) error { _, err := a.Thread(ctx, id, 10); return err },
		"Start": func(a control.API) error {
			_, err := a.Start(ctx, control.StartRequest{Key: "k2", Scope: "s", Thread: "t2"})
			return err
		},
		"Send": func(a control.API) error { return a.Send(ctx, id, "m9", control.Input{Text: "x"}, control.Deliver) },
		"Answer": func(a control.API) error {
			return a.Answer(ctx, session.RequestHandle(sid, "r1"), control.Answer{Text: "x"})
		},
		"Stop":      func(a control.API) error { return a.Stop(ctx, id) },
		"Events":    func(a control.API) error { _, err := a.Events(ctx, control.Filter{Scope: "s"}, 0); return err },
		"Workspace": func(a control.API) error { _, err := a.Workspace(ctx, id); return err },
	}
	if len(ops) != reflect.TypeFor[control.API]().NumMethod() {
		t.Fatal("the test does not cover every operation")
	}
	events, threads, calls := count(t, f.core.Store, "events"), count(t, f.core.Store, "threads"), f.runner.Calls()
	for cname, c := range callers {
		a := f.core.As(c)
		for name, op := range ops {
			f.log.Reset()
			if err := op(a); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s by a caller with %s: %v", name, cname, err)
			}
			if n := strings.Count(f.log.String(), "\n"); n != 1 {
				t.Errorf("%s by a caller with %s wrote %d log lines", name, cname, n)
			}
		}
	}
	if count(t, f.core.Store, "events") != events || count(t, f.core.Store, "threads") != threads || f.runner.Calls() != calls {
		t.Fatal("a refused call changed something")
	}
}

// Workspace reads the directory the thread's session runs in.
func TestWorkspaceOfAThread(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, _ := f.running(t, "t1")
	st, err := f.owner.Workspace(ctx, id)
	if err != nil || st.Branch != "main" || st.Uncommitted != len("dir")+1 {
		t.Fatalf("Workspace: %+v, %v", st, err)
	}
	if _, err := f.owner.Workspace(ctx, control.ThreadID{Scope: "s", Key: "none"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Workspace of no thread: %v", err)
	}
}

func TestStartTwiceWithOneKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	r := control.StartRequest{Key: "k1", Scope: "s", Input: control.Input{Text: "hello"}}
	a, err := f.owner.Start(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.owner.Start(ctx, r)
	if err != nil || a != b {
		t.Fatalf("second Start: %v, %v; first %v", b, err, a)
	}
	if f.runner.Calls() != "start hello" || count(t, f.core.Store, "threads") != 1 {
		t.Fatalf("runner: %q, threads: %d", f.runner.Calls(), count(t, f.core.Store, "threads"))
	}
	var model, effort string
	f.core.Store.DB.QueryRow(`SELECT model, effort FROM sessions`).Scan(&model, &effort)
	if model != "m-default" || effort != "e-default" {
		t.Fatalf("defaults not applied: %s %s", model, effort)
	}
}

func TestStartInDisabledScope(t *testing.T) {
	f := newFixture(t, fakeOwner{key: "new"})
	if _, err := f.owner.Start(context.Background(), control.StartRequest{Key: "k1", Scope: "off"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Start in a disabled scope: %v", err)
	}
	if count(t, f.core.Store, "threads") != 0 {
		t.Fatal("a thread was created")
	}
}

func TestStartAsksTheOwner(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "from-owner"})
	id, err := f.owner.Start(ctx, control.StartRequest{Key: "k1", Scope: "s"})
	if err != nil || id.Key != "from-owner" {
		t.Fatalf("Start: %v, %v", id, err)
	}

	f = newFixture(t, fakeOwner{err: errors.New("cannot open a thread")})
	if _, err := f.owner.Start(ctx, control.StartRequest{Key: "k1", Scope: "s"}); err == nil {
		t.Fatal("Start succeeded although the owner failed")
	}
	if count(t, f.core.Store, "threads") != 0 || count(t, f.core.Store, "inbox") != 0 {
		t.Fatal("a thread exists after the owner failed")
	}
}

func TestSendWhileRunning(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, _ := f.running(t, "t1")
	inputs := func() int {
		var n int
		f.core.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'input'`).Scan(&n)
		return n
	}
	before, calls := inputs(), f.runner.Calls()

	if err := f.owner.Send(ctx, id, "m1", control.Input{Text: "a"}, control.Refuse); !errors.Is(err, ErrBusy) {
		t.Fatalf("Refuse: %v", err)
	}
	if inputs() != before || f.runner.Calls() != calls {
		t.Fatal("a refused Send changed something")
	}
	if err := f.owner.Send(ctx, id, "m2", control.Input{Text: "b"}, control.Deliver); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if err := f.owner.Send(ctx, id, "m3", control.Input{Text: "c"}, control.Interrupt); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if got := strings.TrimPrefix(f.runner.Calls(), calls+"; "); got != "deliver b; interrupt; deliver c" {
		t.Fatalf("runner calls: %q", got)
	}
	if inputs() != before+2 {
		t.Fatalf("%d input events, want %d", inputs(), before+2)
	}
}

func TestThreadDetail(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Text, Text: "first part, "})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.Text, Text: "second part"})
	f.agentEvent(t, sid, 4, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?", Choices: []string{"a", "b"}}})

	d, err := f.owner.Thread(ctx, id, 15)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != control.Waiting || len(d.Requests) != 1 || d.Requests[0].ID != session.RequestHandle(sid, "r1") || len(d.Requests[0].Choices) != 2 {
		t.Fatalf("detail: %+v", d)
	}
	if d.Recent != "rt, second part" {
		t.Fatalf("recent = %q", d.Recent)
	}
}

// The API offers nothing that removes, resets or merges.
func TestNoDestructiveOperation(t *testing.T) {
	typ := reflect.TypeFor[control.API]()
	for i := range typ.NumMethod() {
		name := strings.ToLower(typ.Method(i).Name)
		for _, bad := range []string{"remove", "delete", "reset", "merge", "clean", "purge"} {
			if strings.Contains(name, bad) {
				t.Errorf("control.API has %s", typ.Method(i).Name)
			}
		}
	}
}

func TestRepeatedSendIsIgnoredWhateverBusySays(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, _ := f.running(t, "t1")
	if err := f.owner.Send(ctx, id, "m1", control.Input{Text: "a"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.Calls()
	for _, busy := range []control.Busy{control.Refuse, control.Deliver, control.Interrupt} {
		if err := f.owner.Send(ctx, id, "m1", control.Input{Text: "a"}, busy); err != nil {
			t.Fatalf("repeated Send with %v: %v", busy, err)
		}
	}
	if f.runner.Calls() != calls {
		t.Fatalf("a repeated Send reached the runner: %q", f.runner.Calls())
	}
}

func TestStartKeyIsPerScope(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	f.core.Scopes["s2"] = Scope{Enabled: true, Owner: "chat"}
	a, err := f.owner.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	other := f.core.As(control.Caller{Principal: "y", Scopes: []string{"s2"}, May: all})
	b, err := other.Start(ctx, control.StartRequest{Key: "k1", Scope: "s2", Thread: "t2"})
	if err != nil || b == a || b.Scope != "s2" {
		t.Fatalf("Start in another scope with the same key: %v, %v", b, err)
	}
}

// An event that names threads of other scopes is not shown to a caller
// that may not see them.
func TestEventNamingOtherScopes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, fakeOwner{})
	f.core.Store.CheckEvent = nil // events of the test's own kinds
	f.core.Store.CheckEvent = nil // events of the test's own kinds
	f.core.Store.Update(ctx, func(tx *store.Tx) error {
		tx.Append(ctx, control.Event{Thread: control.ThreadID{Scope: "s", Key: "k"}, Kind: "x", Data: []byte(`{"scopes":["s","hidden"]}`)})
		tx.Append(ctx, control.Event{Thread: control.ThreadID{Scope: "s", Key: "k"}, Kind: "y", Data: []byte(`{"scopes":["s"]}`)})
		return nil
	})
	evs, err := f.core.As(control.Caller{Scopes: []string{"s"}, May: all}).Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := <-evs
	for e.Kind == control.CaughtUp {
		e = <-evs
	}
	if e.Kind != "y" {
		t.Fatalf("caller of scope s got %q", e.Kind)
	}
}

func requestState(t *testing.T, f *fixture, req string) string {
	t.Helper()
	var s string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM requests WHERE request = ?`, req).Scan(&s); err != nil {
		t.Fatalf("request %s: %v", req, err)
	}
	return s
}

func blockedEvents(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	f.core.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'turn_ended' AND json_extract(data, '$.outcome') = 'blocked'`).Scan(&n)
	return n
}

func TestQuestionIsAnsweredOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})

	if d, _ := f.owner.Thread(ctx, id, 0); d.State != control.Waiting {
		t.Fatalf("state %s, want waiting", d.State)
	}
	if s := requestState(t, f, "r1"); s != "open" {
		t.Fatalf("request is %s", s)
	}
	if n := blockedEvents(t, f); n != 1 {
		t.Fatalf("%d blocked events", n)
	}

	if err := f.owner.Answer(ctx, session.RequestHandle(sid, "r1"), control.Answer{Text: "a"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.owner.Thread(ctx, id, 0); d.State != control.Running {
		t.Fatalf("after the answer: %s", d.State)
	}
	calls := f.runner.Calls()
	if !strings.HasSuffix(calls, "answer r1") {
		t.Fatalf("runner calls: %q", calls)
	}
	if err := f.owner.Answer(ctx, session.RequestHandle(sid, "r1"), control.Answer{Text: "b"}); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("second answer: %v", err)
	}

	// A question the agent withdrew cannot be answered.
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded})
	if err := f.owner.Send(ctx, id, "m2", control.Input{Text: "more"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 4, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r2", Text: "sure?"}})
	f.agentEvent(t, sid, 5, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "r2"}})
	if s := requestState(t, f, "r2"); s != "withdrawn" {
		t.Fatalf("withdrawn request is %s", s)
	}
	calls = f.runner.Calls()
	if err := f.owner.Answer(ctx, session.RequestHandle(sid, "r2"), control.Answer{Text: "yes"}); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("answer to a withdrawn request: %v", err)
	}
	if f.runner.Calls() != calls {
		t.Fatal("a refused answer reached the runner")
	}
}

func TestSendAnswersTheOpenQuestion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})
	calls := f.runner.Calls()

	if err := f.owner.Send(ctx, id, "m1", control.Input{Text: "blue"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimPrefix(f.runner.Calls(), calls+"; "); got != "answer r1" {
		t.Fatalf("runner calls: %q", got)
	}
	if s := requestState(t, f, "r1"); s != "answered" {
		t.Fatalf("request is %s", s)
	}
	var text string
	f.core.Store.DB.QueryRow(`SELECT json_extract(data, '$.text') FROM events WHERE kind = 'answer'`).Scan(&text)
	if text != "blue" {
		t.Fatalf("answer event text %q", text)
	}
}

// Agents number their requests per session, so two sessions can ask "q1".
func TestSameRequestIDInTwoSessions(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	_, s1 := f.running(t, "t1")
	_, s2 := f.running(t, "t2")
	f.agentEvent(t, s1, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "q1", Text: "one?"}})
	f.agentEvent(t, s2, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "q1", Text: "two?"}})
	if err := f.owner.Answer(ctx, session.RequestHandle(s1, "q1"), control.Answer{Text: "a"}); err != nil {
		t.Fatal(err)
	}
	var states string
	f.core.Store.DB.QueryRow(`SELECT group_concat(state, ',') FROM (SELECT state FROM requests ORDER BY session_id)`).Scan(&states)
	if states != "answered,open" {
		t.Fatalf("request states by session: %s", states)
	}
}

// A title is cut to 80 bytes at a character boundary; a shorter first
// line is kept as it is.
func TestTitleCutAtACharacter(t *testing.T) {
	long := strings.Repeat("あ", 27) // 81 bytes
	if got := title(long + "\nmore"); !utf8.ValidString(got) || got != strings.Repeat("あ", 26) {
		t.Fatalf("title %q (%d bytes, valid %v)", got, len(got), utf8.ValidString(got))
	}
	short := strings.Repeat("あ", 26) + "ab" // 80 bytes
	if got := title(short + "\nmore"); got != short {
		t.Fatalf("title %q, want %q", got, short)
	}
}
