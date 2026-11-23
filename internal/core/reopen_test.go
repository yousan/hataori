package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

const reopener = closer | control.OpReopen

// Only the owner may reopen: a person of the scope with OpReopen, as a
// chat front end's caller for their press would have, is refused and
// changes nothing, and so is the owner's caller without it.
func TestReopenIsTheOwnersOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	f.core.People = people.New([]people.Person{{Name: "ann", IDs: []string{"chat:ann"}}}, "owner", map[string][]string{"s": {"ann"}})
	id, _ := f.idle(t, "k")
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: reopener})
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.DB.Exec(`UPDATE threads SET workspace = ? WHERE id = ?`, t.TempDir(), int64(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id = 'conv-k', spec = '{}'`); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]control.Caller{
		"a person of the scope": {From: "chat:ann", Scopes: []string{"s"}, May: control.OpRead | control.OpReopen},
		"the owner without it":  {Principal: "owner", Scopes: []string{"s"}, May: closer},
	} {
		f.log.Reset()
		if _, err := f.core.As(c).ReopenThread(ctx, id); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v, want forbidden", name, err)
		}
		if n := strings.Count(f.log.String(), "\n"); n != 1 {
			t.Errorf("%s: %d log lines", name, n)
		}
		if f.threadState(t, id) != control.Closed || len(f.eventsOf(t, id, KindReopened)) != 0 {
			t.Fatalf("%s: a refused reopen changed the thread", name)
		}
	}
	got, err := owner.ReopenThread(ctx, id)
	if err != nil || got != (control.Reopened{Workspace: control.ReopenKept, Conversation: control.ReopenResume}) {
		t.Fatalf("the owner's reopen: %v, %v", got, err)
	}
	if f.threadState(t, id) != control.Asleep || len(f.eventsOf(t, id, KindReopened)) != 1 {
		t.Fatalf("after the owner's reopen: %s", f.threadState(t, id))
	}
}

// A closed supervisor thread (one started confined) is reopened only while
// no other supervisor thread of its scope is open: the refusal names the
// open one. Once that one is closed, it is reopened, and it is the scope's
// supervisor thread again, though a newer one exists.
func TestReopenASupervisorThreadOnlyWhenNoOtherIsOpen(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	f.core.Workspaces = confinedWorkspaces{}
	dir := filepath.Join(t.TempDir(), "scope")
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: reopener})
	start := func(key string) control.ThreadID {
		id, err := f.core.StartConfined(ctx, control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
			control.StartRequest{Key: key, Scope: "s", Thread: key, Input: control.Input{Text: "go"}}, dir)
		if err != nil {
			t.Fatal(err)
		}
		sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady, SessionID: "conv-" + key})
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET spec = '{}' WHERE id = ?`, sid); err != nil { // as a launch stores it
			t.Fatal(err)
		}
		return id
	}
	first := start("sv")
	if err := owner.CloseThread(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := start("sv-after")

	_, err := owner.ReopenThread(ctx, first)
	var refusal *control.ReopenRefused
	if !errors.As(err, &refusal) || !errors.Is(err, control.ErrReopenRefused) ||
		!strings.Contains(refusal.Reason, fmt.Sprintf("thread %d", second)) {
		t.Fatalf("reopen while thread %d is open: %v", second, err)
	}
	if f.threadState(t, first) != control.Closed || len(f.eventsOf(t, first, KindReopened)) != 0 {
		t.Fatal("a refused reopen changed the thread")
	}

	if err := owner.CloseThread(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ReopenThread(ctx, first); err != nil {
		t.Fatal(err)
	}
	if id, state, err := f.core.LastConfined(ctx, "s"); err != nil || id != first || state != control.Asleep {
		t.Fatalf("the scope's supervisor thread: %d %s %v, want the reopened %d", id, state, err, first)
	}
	// And the newer one is now refused in turn.
	if _, err := owner.ReopenThread(ctx, second); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, fmt.Sprintf("thread %d", first)) {
		t.Fatalf("reopen of the newer one: %v", err)
	}
}

// A confined thread does not start while another confined thread of its
// scope is open, as when a reopen took it back first; once that one is
// closed, it does.
func TestNoConfinedStartWhileAnotherIsOpen(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	f.core.Workspaces = confinedWorkspaces{}
	dir := filepath.Join(t.TempDir(), "scope")
	caller := control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all}
	first, err := f.core.StartConfined(ctx, caller, control.StartRequest{Key: "sv", Scope: "s", Thread: "sv", Input: control.Input{Text: "go"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.core.StartConfined(ctx, caller, control.StartRequest{Key: "sv-after", Scope: "s", Thread: "sv2", Input: control.Input{Text: "go"}}, dir)
	if !errors.Is(err, session.ErrRefused) || !strings.Contains(err.Error(), fmt.Sprintf("thread %d", first)) {
		t.Fatalf("a second confined start: %v", err)
	}
	if id, _, _ := f.core.LastConfined(ctx, "s"); id != first {
		t.Fatalf("the scope's confined thread is %d, want %d", id, first)
	}
	sid, _, _ := f.core.current(ctx, f.core.Store.DB, first)
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
	if err := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer}).CloseThread(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.StartConfined(ctx, caller, control.StartRequest{Key: "sv-after", Scope: "s", Thread: "sv2", Input: control.Input{Text: "go"}}, dir); err != nil {
		t.Fatalf("a confined start once the other is closed: %v", err)
	}
}

// A closed confined thread whose directory is gone gets it made again,
// empty, and a new conversation; a thread whose agent was never started is
// refused and changes nothing.
func TestReopenMakesAConfinedDirectoryAgain(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	f.core.Workspaces = confinedWorkspaces{}
	dir := filepath.Join(t.TempDir(), "scope")
	id, err := f.core.StartConfined(ctx, control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "sv", Scope: "s", Thread: "sv", Input: control.Input{Text: "go"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	sid, _, _ := f.core.current(ctx, f.core.Store.DB, id)
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady, SessionID: "conv"})
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: reopener})
	if err := owner.CloseThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	var refusal *control.ReopenRefused
	if _, err := owner.ReopenThread(ctx, id); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "never started") ||
		f.threadState(t, id) != control.Closed {
		t.Fatalf("reopen of a thread never launched: %v, state %s", err, f.threadState(t, id))
	}
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET spec = '{}'`); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	got, err := owner.ReopenThread(ctx, id)
	if err != nil || got != (control.Reopened{Workspace: control.ReopenRemade, Conversation: control.ReopenNew}) {
		t.Fatalf("reopen with the directory gone: %v, %v", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the directory made again: %v, %v", entries, err)
	}
	// Until a message starts the new conversation, an attach is refused
	// and leaves the thread asleep.
	_, err = f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: control.OpAttach | control.OpRead}).AttachTerminal(ctx, id)
	if !errors.Is(err, session.ErrRefused) || f.threadState(t, id) != control.Asleep {
		t.Fatalf("an attach before the first message: %v, state %s", err, f.threadState(t, id))
	}
}
