package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/session"
)

// A caller limited to Threads and Events is refused every other operation,
// each with one log line naming the operation, the scope and the caller,
// and nothing changes; Threads and Events answer. The same caller without
// the limit makes every one of those calls: only the limit refuses them.
func TestLimitedCallerGetsOnlyItsOperations(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})

	open := control.Caller{Principal: "owner", Scopes: []string{"s"}, May: control.Ops(^uint16(0))}
	limited := open
	limited.Limit = &control.Limit{Operations: []string{"Threads", "Events"}, Events: []control.EventKind{"turn_started"}}

	refused := []struct {
		op   string
		call func(control.API) error
	}{
		{"Thread", func(a control.API) error { _, err := a.Thread(ctx, id, 10, 10); return err }},
		{"Workspace", func(a control.API) error { _, err := a.Workspace(ctx, id); return err }},
		{"Terminal", func(a control.API) error {
			ch, err := a.Terminal(ctx, id, "", 0)
			if err == nil {
				for range ch {
				}
			}
			return err
		}},
		{"Limits", func(a control.API) error { _, err := a.Limits(ctx); return err }},
		{"Start", func(a control.API) error {
			_, err := a.Start(ctx, control.StartRequest{Key: "k2", Scope: "s", Thread: "t2", Input: control.Input{Text: "x"}})
			return err
		}},
		// Answer before Send: a Send to a thread with a question open
		// would answer it.
		{"Answer", func(a control.API) error {
			return a.Answer(ctx, session.RequestHandle(sid, "r1"), control.Answer{Text: "x"})
		}},
		{"Send", func(a control.API) error { return a.Send(ctx, id, "m9", control.Input{Text: "x"}, control.Deliver) }},
		{"Stop", func(a control.API) error { return a.Stop(ctx, id) }},
	}
	events, threads, calls := count(t, f.core.Store, "events"), count(t, f.core.Store, "threads"), f.runner.Calls()
	for _, r := range refused {
		f.log.Reset()
		if err := r.call(f.core.As(limited)); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s by a caller limited to Threads and Events: %v", r.op, err)
		}
		lines := strings.Split(strings.TrimSpace(f.log.String()), "\n")
		if len(lines) != 1 || !strings.Contains(lines[0], "control call refused") || !strings.Contains(lines[0], "op="+r.op+" ") ||
			!strings.Contains(lines[0], `scope=`) || !strings.Contains(lines[0], "principal=owner") {
			t.Errorf("%s: the refusal logged %q", r.op, f.log.String())
		}
		if r.op != "Limits" && !strings.Contains(lines[0], "scope=s ") {
			t.Errorf("%s: the refusal names no scope: %q", r.op, lines[0])
		}
	}
	if count(t, f.core.Store, "events") != events || count(t, f.core.Store, "threads") != threads || f.runner.Calls() != calls {
		t.Fatal("a refused call changed something")
	}

	a := f.core.As(limited)
	if ts, err := a.Threads(ctx, control.Filter{Scope: "s"}); err != nil || len(ts) != 1 || ts[0].ID != id {
		t.Fatalf("Threads: %v, %v", ts, err)
	}
	ectx, cancel := context.WithCancel(ctx)
	ch, err := a.Events(ectx, control.Filter{Scope: "s"}, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	cancel()
	for range ch {
	}

	for _, r := range refused {
		if err := r.call(f.core.As(open)); err != nil {
			t.Errorf("%s by the same caller without the limit: %v", r.op, err)
		}
	}
}

// A caller without a limit, of every kind there is, is refused nothing
// for a limit: the operations are named as the protocol names them, and
// Close and Remove, whose methods have other names, too.
func TestLimitNamesOperationsAsTheProtocol(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, _ := f.running(t, "t1")
	c := control.Caller{Principal: "owner", Scopes: []string{"s"}, May: control.Ops(^uint16(0)),
		Limit: &control.Limit{Operations: []string{"Close"}}}
	f.log.Reset()
	// Close is granted: it is refused for the running turn, not the limit.
	if err := f.core.As(c).CloseThread(ctx, id); errors.Is(err, ErrForbidden) || err == nil {
		t.Fatalf("Close granted: %v", err)
	}
	if strings.Contains(f.log.String(), "not granted") {
		t.Fatalf("Close granted was refused for the limit: %s", f.log.String())
	}
	f.log.Reset()
	if err := f.core.As(c).RemoveThread(ctx, id); !errors.Is(err, ErrForbidden) || !strings.Contains(f.log.String(), "op=Remove ") {
		t.Fatalf("Remove not granted: %v, %s", err, f.log.String())
	}
}

// A caller granted the kinds turn_started and turn_ended reads only those
// from a position, through Events, ThreadEvents and Thread alike, at the
// positions the instance gave them. After a reconnect from the last
// position it read, it reads the next events of those kinds, none skipped
// and none twice.
func TestLimitedCallerReadsOnlyItsEventKinds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	kinds := []control.EventKind{"turn_started", "turn_ended"}
	c := control.Caller{Principal: "p", Scopes: []string{"s"}, May: control.OpRead,
		Limit: &control.Limit{Operations: []string{"Events", "ThreadEvents", "Thread"}, Events: kinds}}
	a := f.core.As(c)

	turn := func(sid int64, seq uint64) {
		f.agentEvent(t, sid, seq, agent.Event{Kind: agent.TurnStarted})
		f.agentEvent(t, sid, seq+1, agent.Event{Kind: agent.Text, Text: "out"})
		f.agentEvent(t, sid, seq+2, agent.Event{Kind: agent.ToolStarted, ID: "u1", Tool: "Bash"})
		f.agentEvent(t, sid, seq+3, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	}
	id, sid := f.running(t, "t1")
	turn(sid, 2)

	// want is what the owner reads after pos, of the granted kinds.
	want := func(after uint64) []control.Event {
		var out []control.Event
		for _, e := range read(t, f.owner, after) {
			if slices.Contains(kinds, e.Kind) {
				out = append(out, e)
			}
		}
		return out
	}
	got := read(t, a, 0)
	if len(got) < 2 || !reflect.DeepEqual(got, want(0)) {
		t.Fatalf("events read:\n%v\nwant:\n%v", got, want(0))
	}
	if all := read(t, f.owner, 0); len(all) <= len(got) {
		t.Fatalf("the owner reads %d events, the limited caller %d: nothing was left out", len(all), len(got))
	}
	last := got[len(got)-1].Pos

	// Events of other kinds, then of the granted ones, while it is away.
	f.agentEvent(t, sid, 20, agent.Event{Kind: agent.Text, Text: "more"})
	if err := f.owner.Send(ctx, id, "again", control.Input{Text: "again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	turn(sid, 21)
	again := read(t, a, last)
	if len(again) < 2 || !reflect.DeepEqual(again, want(last)) {
		t.Fatalf("after a reconnect from %d:\n%v\nwant:\n%v", last, again, want(last))
	}
	if again[0].Pos <= last {
		t.Fatalf("an event read twice: %v after %d", again[0], last)
	}

	te, err := a.ThreadEvents(ctx, id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.Thread(ctx, id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	for _, evs := range [][]control.Event{te, d.Events} {
		if !reflect.DeepEqual(evs, want(0)) {
			t.Fatalf("thread events:\n%v\nwant:\n%v", evs, want(0))
		}
	}
	// A page of ThreadEvents is n events of the granted kinds, not n events.
	if page, err := a.ThreadEvents(ctx, id, 0, 2); err != nil || !reflect.DeepEqual(page, want(0)[len(want(0))-2:]) {
		t.Fatalf("a page of 2: %v, %v", page, err)
	}
}

// read returns the stored events after pos that api reads, up to the
// CaughtUp mark, and ends the stream: a reader that goes away.
func read(t *testing.T, api control.API, after uint64) []control.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := api.Events(ctx, control.Filter{}, after)
	if err != nil {
		t.Fatal(err)
	}
	var out []control.Event
	for e := range ch {
		if e.Kind == control.CaughtUp {
			cancel()
			for range ch {
			}
			break
		}
		out = append(out, e)
	}
	return out
}

// The caller of a plugin without the "conversation" grant: a marker in an
// input, in the agent's text, in a question's text and in an approval's
// reason appears in nothing it reads through Threads, Thread and Events,
// and Terminal is refused (#263). The same plugin with the grant reads
// them: the marker is there to be left out.
func TestPluginWithoutConversationReadsNoText(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	const marker = "MARKER-7c1e"
	id, err := f.owner.Start(ctx, control.StartRequest{Key: "t1", Scope: "s", Thread: "t1", Input: control.Input{Text: "start " + marker}})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
	if err := f.owner.Send(ctx, id, "m1", control.Input{Text: "input " + marker}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnStarted})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.Text, Text: "text " + marker})
	// An approval asked in this thread, about another thread's running turn.
	other, _ := f.running(t, "t2")
	asker := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: id})
	if _, err := asker.AskInterrupt(ctx, other, "ask", "reason "+marker); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 4, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "question " + marker, Choices: []string{"yes " + marker}}})

	g := plugin.Grant{ID: "scan", Scopes: []string{"s"},
		Operations: []string{"Threads", "Thread", "Events", "Terminal"},
		Events:     control.KindNames()}
	seen := func(c control.Caller) string {
		a := f.core.As(c)
		ts, err := a.Threads(ctx, control.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		var d []control.ThreadDetail
		for _, ti := range ts {
			x, err := a.Thread(ctx, ti.ID, 1<<20, control.MaxEvents)
			if err != nil {
				t.Fatal(err)
			}
			d = append(d, x)
		}
		b, _ := json.Marshal([]any{ts, d, read(t, a, 0)})
		return string(b)
	}

	if got := seen(g.Caller()); strings.Contains(got, marker) {
		t.Errorf("a plugin without \"conversation\" read the marker: %s", got)
	}
	f.log.Reset()
	if _, err := f.core.As(g.Caller()).Terminal(ctx, id, "", 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("Terminal by a plugin without \"conversation\": %v", err)
	}
	if !strings.Contains(f.log.String(), "op=Terminal") {
		t.Errorf("the refused Terminal logged %q", f.log.String())
	}

	g.Conversation = true
	got := seen(g.Caller())
	for _, s := range []string{"start ", "input ", "text ", "reason ", "question ", "yes "} {
		if !strings.Contains(got, s+marker) {
			t.Errorf("the plugin with \"conversation\" does not read %q: the test shows nothing", s+marker)
		}
	}
}

// The owner's caller that may type into host panes, limited to operations
// that do not name OperateHost, is refused it with one log line, and tmux
// is not read.
func TestLimitedOperatorMayNotOperateHost(t *testing.T) {
	in := &fakeInput{}
	f := inputFixture(t, in)
	c := operator
	c.Limit = &control.Limit{Operations: []string{"HostTerminals", "HostScreen", "HostKeys", "End"}}
	if op, err := f.core.As(c).OperateHost(context.Background(), "%1"); !errors.Is(err, ErrForbidden) || op != nil {
		t.Fatalf("%v, %v", op, err)
	}
	if lines := logLines(f, "op=OperateHost"); len(lines) != 1 || !strings.Contains(lines[0], "not granted") {
		t.Errorf("log %q", f.log.String())
	}
	if len(in.checks) != 0 {
		t.Errorf("a refused caller had tmux read %d times", len(in.checks))
	}
	if op, err := f.core.As(operator).OperateHost(context.Background(), "%1"); err != nil {
		t.Fatalf("the same caller without the limit: %v", err)
	} else {
		op.End()
	}
}

// A caller not granted an operation on a thread is refused it alike, with
// one log line, whether the thread exists or not; and the host's
// terminals, which only the owner reads, are refused to it with the line
// naming who it is.
func TestLimitedCallerLearnsNothingOfThreads(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	f.core.HostTerms = &fakeHostTerms{}
	id, _ := f.running(t, "t1")
	c := control.Caller{From: "plugin:scan", Scopes: []string{"s"}, May: control.Ops(^uint16(0)),
		Limit: &control.Limit{Operations: []string{"Threads"}}}
	a := f.core.As(c)
	for _, th := range []control.ThreadID{id, 999} {
		for name, call := range map[string]func() error{
			"Thread":         func() error { _, err := a.Thread(ctx, th, 0, 0); return err },
			"ThreadEvents":   func() error { _, err := a.ThreadEvents(ctx, th, 0, 1); return err },
			"Workspace":      func() error { _, err := a.Workspace(ctx, th); return err },
			"Send":           func() error { return a.Send(ctx, th, "k", control.Input{Text: "x"}, control.Refuse) },
			"Stop":           func() error { return a.Stop(ctx, th) },
			"Remove":         func() error { return a.RemoveThread(ctx, th) },
			"AttachTerminal": func() error { _, err := a.AttachTerminal(ctx, th); return err },
			"Link": func() error {
				_, err := a.Link(ctx, th, control.LinkRef{Client: "elsewhere", Channel: "x"})
				return err
			},
			"Link ": func() error { _, err := a.Link(ctx, th, control.LinkRef{}); return err },
			"Events": func() error {
				_, err := a.Events(ctx, control.Filter{Scope: "elsewhere", Thread: th}, 0)
				return err
			},
			"SetName": func() error {
				return a.SetName(ctx, control.LinkRef{Client: "chat", Channel: "s", Thread: fmt.Sprint("no-", th)}, "n")
			},
		} {
			f.log.Reset()
			if err := call(); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s of thread %d: %v", name, th, err)
			}
			if lines := logLines(f, "not granted"); len(lines) != 1 || strings.Count(f.log.String(), "\n") != 1 || !strings.Contains(lines[0], "op="+strings.TrimSpace(name)+" ") {
				t.Errorf("%s of thread %d logged %q", name, th, f.log.String())
			}
		}
	}
	for name, call := range map[string]func() error{
		"HostTerminals": func() error { _, err := a.HostTerminals(ctx); return err },
		"HostScreen":    func() error { _, err := a.HostScreen(ctx, "%1"); return err },
	} {
		f.log.Reset()
		if err := call(); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
		if lines := logLines(f, "op="+name+" "); len(lines) != 1 || !strings.Contains(lines[0], "from=plugin:scan") {
			t.Errorf("%s logged %q", name, f.log.String())
		}
	}
}
