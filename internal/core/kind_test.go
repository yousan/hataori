package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

// kinds starts and reopens confined threads of scope s.
type kinds struct {
	*fixture
	dir   string
	owner control.API
}

func newKinds(t *testing.T) *kinds {
	t.Helper()
	f := newFixture(t, fakeOwner{})
	f.core.Workspaces = confinedWorkspaces{}
	return &kinds{f, filepath.Join(t.TempDir(), "scope"),
		f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: reopener | control.OpStart | control.OpRead})}
}

func (k *kinds) try(kind control.ThreadKind, key string) (control.ThreadID, error) {
	return k.core.StartConfined(context.Background(), control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: key, Scope: "s", Thread: key, Input: control.Input{Text: "go"}}, kind, k.dir)
}

// start starts a thread of kind whose agent was launched, so that it can be
// reopened.
func (k *kinds) start(t *testing.T, kind control.ThreadKind, key string) control.ThreadID {
	t.Helper()
	id, err := k.try(kind, key)
	if err != nil {
		t.Fatalf("starting a %s thread: %v", kind, err)
	}
	sid, _, err := k.core.current(context.Background(), k.core.Store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	k.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady, SessionID: "conv-" + key})
	if _, err := k.core.Store.DB.Exec(`UPDATE sessions SET spec = '{}' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	return id
}

func (k *kinds) close(t *testing.T, id control.ThreadID) {
	t.Helper()
	if err := k.owner.CloseThread(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func (k *kinds) supervisor(t *testing.T, want control.ThreadID, state control.State, when string) {
	t.Helper()
	if id, got, err := k.core.SupervisorThread(context.Background(), "s"); err != nil || id != want || got != state {
		t.Fatalf("%s: the scope's supervisor thread is %d %s, %v; want %d %s", when, id, got, err, want, state)
	}
}

// A scope's supervisor thread is its own one, whatever conversations and
// look-ups are older or newer, open or closed (#867).
func TestSupervisorThreadIsTheOwnOne(t *testing.T) {
	k := newKinds(t)
	older := k.start(t, control.KindConversation, "c1")
	k.supervisor(t, 0, "", "with a conversation only")
	own := k.start(t, control.KindSupervisor, "sv")
	k.supervisor(t, own, control.Idle, "with an older conversation")
	newer := k.start(t, control.KindConversation, "c2")
	k.start(t, control.KindLookup, "l1")
	k.supervisor(t, own, control.Idle, "with a newer conversation and a look-up")
	k.close(t, own)
	k.supervisor(t, own, control.Closed, "closed, with both open")
	k.close(t, older)
	k.close(t, newer)
	k.supervisor(t, own, control.Closed, "all closed")
	second := k.start(t, control.KindSupervisor, "sv-after")
	k.supervisor(t, second, control.Idle, "replaced")
	k.start(t, control.KindConversation, "c3")
	k.close(t, second)
	k.supervisor(t, second, control.Closed, "both own ones closed")
	if _, err := k.owner.ReopenThread(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	k.supervisor(t, own, control.Asleep, "the older own one reopened")
}

// One of a supervisor's own threads of a scope is open at a time: a second
// one's start and a closed one's reopen name it. A conversation starts and
// is reopened beside it and beside others, and neither holds an own one's
// start or reopen up; a look-up starts beside all and is never reopened.
func TestOneOwnThreadOpenBesideConversations(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	c1 := k.start(t, control.KindConversation, "c1")
	own := k.start(t, control.KindSupervisor, "sv")
	c2 := k.start(t, control.KindConversation, "c2")
	look := k.start(t, control.KindLookup, "l1")

	if _, err := k.try(control.KindSupervisor, "sv2"); !errors.Is(err, session.ErrRefused) || !strings.Contains(err.Error(), fmt.Sprintf("thread %d", own)) {
		t.Fatalf("a second own thread's start: %v", err)
	}
	for _, id := range []control.ThreadID{c1, c2} {
		k.close(t, id)
		if _, err := k.owner.ReopenThread(ctx, id); err != nil {
			t.Fatalf("a conversation's reopen: %v", err)
		}
	}
	k.close(t, look)
	var refusal *control.ReopenRefused
	if _, err := k.owner.ReopenThread(ctx, look); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "look-up") {
		t.Fatalf("a look-up's reopen: %v", err)
	}
	if k.threadState(t, look) != control.Closed || len(k.eventsOf(t, look, KindReopened)) != 0 {
		t.Fatal("a refused reopen changed the look-up")
	}

	k.close(t, own)
	second := k.start(t, control.KindSupervisor, "sv2") // with both conversations open
	if _, err := k.owner.ReopenThread(ctx, own); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, fmt.Sprintf("thread %d", second)) {
		t.Fatalf("a closed own thread's reopen: %v", err)
	}
	k.close(t, second)
	if _, err := k.owner.ReopenThread(ctx, own); err != nil { // with both conversations open
		t.Fatal(err)
	}
}

// A thread keeps the kind it was started with: a key that started one of
// one kind starts none of another, and no thread is started confined as an
// ordinary one or as one of a kind there is not.
func TestAKeyStartsOneKind(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	confined := []control.ThreadKind{control.KindSupervisor, control.KindConversation, control.KindLookup}
	for _, kind := range confined {
		id := k.start(t, kind, "of "+string(kind))
		if again, err := k.try(kind, "of "+string(kind)); err != nil || again != id {
			t.Fatalf("a %s thread's key again: %d, %v; want %d", kind, again, err, id)
		}
		for _, other := range confined {
			if other == kind {
				continue
			}
			if _, err := k.try(other, "of "+string(kind)); !errors.Is(err, ErrKindKey) {
				t.Errorf("a %s thread's key for a %s: %v", kind, other, err)
			}
		}
		if _, err := k.owner.Start(ctx, control.StartRequest{Key: "of " + string(kind), Scope: "s", Thread: "t", Input: control.Input{Text: "go"}}); !errors.Is(err, ErrKindKey) {
			t.Errorf("a %s thread's key for an ordinary start: %v", kind, err)
		}
	}
	if _, err := k.owner.Start(ctx, control.StartRequest{Key: "ordinary", Scope: "s", Thread: "o", Input: control.Input{Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.try(control.KindConversation, "ordinary"); !errors.Is(err, ErrKindKey) {
		t.Errorf("an ordinary thread's key for a conversation: %v", err)
	}
	threads := count(t, k.core.Store, "threads")
	for _, kind := range []control.ThreadKind{"", control.KindOrdinary, "other"} {
		if id, err := k.try(kind, "new "+string(kind)); err == nil || id != 0 {
			t.Errorf("started confined as %q: %d, %v", kind, id, err)
		}
	}
	if n := count(t, k.core.Store, "threads"); n != threads {
		t.Fatalf("%d threads, want %d", n, threads)
	}
}

// Threads, Thread and thread_created give the kind, also of a thread that a
// take-in or an approved proposal made.
func TestThreadsGiveTheirKind(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	want := map[control.ThreadID]control.ThreadKind{}
	for _, kind := range []control.ThreadKind{control.KindSupervisor, control.KindConversation, control.KindLookup} {
		want[k.start(t, kind, string(kind))] = kind
	}
	id, err := k.owner.Start(ctx, control.StartRequest{Key: "o", Scope: "s", Thread: "o", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	want[id] = control.KindOrdinary
	ths, err := k.owner.Threads(ctx, control.Filter{Scope: "s"})
	if err != nil || len(ths) != len(want) {
		t.Fatalf("%d threads, %v", len(ths), err)
	}
	for _, th := range ths {
		d, err := k.owner.Thread(ctx, th.ID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		made := k.eventsOf(t, th.ID, "thread_created")
		if kind := want[th.ID]; th.Kind != kind || d.Kind != kind || len(made) != 1 || made[0]["kind"] != string(kind) || th.Confined != kind.Confined() {
			t.Errorf("thread %d: Threads %q, Thread %q, %v, confined %v; want %q", th.ID, th.Kind, d.Kind, made, th.Confined, kind)
		}
	}

	ordinary := func(f *fixture, id control.ThreadID, how string) {
		t.Helper()
		got, err := f.core.Kind(ctx, id)
		made := f.eventsOf(t, id, "thread_created")
		if err != nil || got != control.KindOrdinary || len(made) != 1 || made[0]["kind"] != string(control.KindOrdinary) {
			t.Errorf("%s made a %q thread, %v, %v; want an ordinary one", how, got, made, err)
		}
	}
	f, _, owner := takeInFixture(t)
	if id, err = owner.TakeIn(ctx, control.TakeInRequest{Window: takeWindow, Scope: "s", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	ordinary(f, id, "a take-in")
	f, proposer, asksIn, _ := proposalFixture(t)
	_, err = proposer.Start(ctx, control.StartRequest{Key: "p", Scope: "s", Input: control.Input{Text: "do it"}})
	proposalOf(t, err)
	d, err := f.owner.Thread(ctx, asksIn, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("requests: %+v %v", d.Requests, err)
	}
	before := count(t, f.core.Store, "threads")
	if err := f.owner.Answer(ctx, d.Requests[0].ID, control.Answer{Choices: []string{"approve"}}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f.core.Store, "threads"); n != before+1 {
		t.Fatalf("%d threads, want %d", n, before+1)
	}
	var last int64
	f.core.Store.DB.QueryRow(`SELECT max(id) FROM threads`).Scan(&last)
	ordinary(f, control.ThreadID(last), "an approved proposal")
}
