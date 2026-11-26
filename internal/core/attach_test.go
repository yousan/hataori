package core

import (
	"context"
	"errors"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

// asleep starts a thread, ends its turn and puts it to sleep.
func (f *fixture) asleep(t *testing.T, key string) (control.ThreadID, int64) {
	t.Helper()
	id, sid := f.running(t, key)
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnStarted})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded})
	if err := f.core.Machine.Apply(context.Background(), sid, session.Input{Kind: session.IdleTimeout}); err != nil {
		t.Fatal(err)
	}
	return id, sid
}

func (f *fixture) state(t *testing.T, sid int64) control.State {
	t.Helper()
	var s string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, sid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return control.State(s)
}

// Only the owner's caller with OpAttach may attach a terminal. A refused
// attach, and one to a closed thread or an agent without a terminal,
// leaves the thread as it was and starts nothing.
func TestAttachTerminalRefusalsChangeNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.asleep(t, "t1")
	refusals := map[string]struct {
		caller control.Caller
		pipes  bool
		want   error
	}{
		"not the owner":     {control.Caller{Principal: "someone", Scopes: []string{"s"}, May: all | control.OpAttach}, false, ErrForbidden},
		"a guest":           {control.Caller{From: "chat:u9", Scopes: []string{"s"}, May: all | control.OpAttach}, false, ErrForbidden},
		"without OpAttach":  {control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all}, false, ErrForbidden},
		"an agent on pipes": {control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpAttach}, true, session.ErrRefused},
	}
	for name, c := range refusals {
		events, calls := count(t, f.core.Store, "events"), f.runner.Calls()
		f.runner.pipes = c.pipes
		_, err := f.core.As(c.caller).AttachTerminal(ctx, id)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if count(t, f.core.Store, "events") != events || f.runner.Calls() != calls || f.state(t, sid) != control.Asleep {
			t.Errorf("%s: the refused attach changed something", name)
		}
	}
	f.runner.pipes = false

	if err := f.core.Machine.Apply(ctx, sid, session.Input{Kind: session.Close}); err != nil {
		t.Fatal(err)
	}
	events, calls := count(t, f.core.Store, "events"), f.runner.Calls()
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpAttach})
	if _, err := owner.AttachTerminal(ctx, id); !errors.Is(err, ErrClosed) {
		t.Fatalf("attach to a closed thread: %v", err)
	}
	if count(t, f.core.Store, "events") != events || f.runner.Calls() != calls || f.state(t, sid) != control.Closed {
		t.Fatal("an attach to a closed thread changed something")
	}
}

// The owner's attach wakes an asleep thread with no input, and names the
// socket of its session host.
func TestAttachTerminalWakesWithoutInput(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.asleep(t, "t1")
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id = 'conv'`); err != nil { // as the agent reports it
		t.Fatal(err)
	}
	before := f.runner.Calls()
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpAttach})
	at, err := owner.AttachTerminal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if at.Socket != "socket 1" || at.Number != 1 {
		t.Fatalf("attachment %+v", at)
	}
	if got := f.runner.Calls(); got != before+"; wake 1" {
		t.Fatalf("runner calls %q after %q, want one wake", got, before)
	}
	if f.state(t, sid) != control.Creating {
		t.Fatalf("state %s, want creating until the agent reports it is ready", f.state(t, sid))
	}
	var fi string
	var attachRequested int64
	if err := f.core.Store.DB.QueryRow(`SELECT first_input, attach_requested_at FROM sessions WHERE id = ?`, sid).Scan(&fi, &attachRequested); err != nil {
		t.Fatal(err)
	}
	if fi != `{"principal":"owner","key":"","text":"","wake":true,"no_input":true}` {
		t.Fatalf("first input %s", fi)
	}
	if attachRequested <= 0 {
		t.Fatal("wake lost the terminal attach reservation")
	}
}

// Attaching to an idle thread starts its idle period again, so that it is
// not put to sleep before the terminal reaches its host.
func TestAttachTerminalRestartsTheIdlePeriod(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnStarted})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded})
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET updated_at = 1 WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpAttach})
	if _, err := owner.AttachTerminal(ctx, id); err != nil {
		t.Fatal(err)
	}
	var updated, attachRequested int64
	if err := f.core.Store.DB.QueryRow(`SELECT updated_at, attach_requested_at FROM sessions WHERE id = ?`, sid).Scan(&updated, &attachRequested); err != nil {
		t.Fatal(err)
	}
	if updated <= 1 || f.state(t, sid) != control.Idle {
		t.Fatalf("after the attach: updated_at %d, state %s; want the idle period started again", updated, f.state(t, sid))
	}
	if attachRequested != updated {
		t.Fatalf("attach reservation %d, updated %d", attachRequested, updated)
	}
}
