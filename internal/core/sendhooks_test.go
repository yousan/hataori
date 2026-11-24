package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// countHooks lets everything pass and counts its calls.
type countHooks struct{ n atomic.Int64 }

func (h *countHooks) Run(_ context.Context, c control.HookCall) control.HookResult {
	h.n.Add(1)
	return control.HookResult{Text: c.Text}
}

// A closed thread that is reopened between the two reads of its state in a
// Send does not get the input: no hook ran for it (#720). Once it is open,
// the same key is taken, through its hooks, and repeated it runs none.
func TestSendToAThreadReopenedMeanwhileIsNotTakenWithoutItsHooks(t *testing.T) {
	ctx := context.Background()
	for name, guard := range map[string]*sendGuard{"a Send": nil, "an approved proposal's": {proposal: true}} {
		t.Run(name, func(t *testing.T) {
			f, id := closedThread(t)
			hooks := &countHooks{}
			f.core.Hooks = hooks
			owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | reopener})
			f.core.stateRead = func() {
				f.core.stateRead = nil
				if _, err := owner.ReopenThread(ctx, id); err != nil {
					t.Error(err)
				}
			}
			inbox := count(t, f.core.Store, "inbox")
			send := func() error {
				return owner.(*api).send(ctx, id, "m1", control.Input{Text: "wake up"}, control.Deliver, guard)
			}
			if err := send(); !errors.Is(err, ErrClosed) {
				t.Fatalf("the send: %v, want ErrClosed", err)
			}
			if f.threadState(t, id) != control.Asleep || count(t, f.core.Store, "inbox") != inbox || hooks.n.Load() != 0 {
				t.Fatalf("the thread is %s, %d inbox rows (had %d), %d hook calls: want it reopened and nothing taken",
					f.threadState(t, id), count(t, f.core.Store, "inbox"), inbox, hooks.n.Load())
			}
			if err := send(); err != nil {
				t.Fatal(err)
			}
			if f.threadState(t, id) == control.Asleep || hooks.n.Load() != 2 {
				t.Fatalf("sent again: the thread is %s, %d hook calls, want it woken and the 2 points run", f.threadState(t, id), hooks.n.Load())
			}
			if err := send(); err != nil || hooks.n.Load() != 2 {
				t.Fatalf("the key repeated: %v, %d hook calls", err, hooks.n.Load())
			}
		})
	}
}

// A Send tied to the running turn finds that turn over, in a closed thread
// and in one reopened meanwhile.
func TestGuardedSendToAClosedThreadFindsItsTurnOver(t *testing.T) {
	ctx := context.Background()
	for _, reopened := range []bool{false, true} {
		f, id := closedThread(t)
		hooks := &countHooks{}
		f.core.Hooks = hooks
		owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | reopener})
		if reopened {
			f.core.stateRead = func() {
				if _, err := owner.ReopenThread(ctx, id); err != nil {
					t.Error(err)
				}
			}
		}
		sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		err = owner.(*api).send(ctx, id, "m1", control.Input{Text: "stop"}, control.Interrupt, &sendGuard{session: sid, turn: 1})
		if !errors.Is(err, errGuardEnded) || hooks.n.Load() != 0 {
			t.Fatalf("reopened %v: %v, %d hook calls", reopened, err, hooks.n.Load())
		}
	}
}

// closedThread is a closed thread that can be reopened.
func closedThread(t *testing.T) (*fixture, control.ThreadID) {
	t.Helper()
	f := newFixture(t, fakeOwner{})
	id, _ := f.idle(t, "k")
	if err := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: closer}).CloseThread(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.DB.Exec(`UPDATE threads SET workspace = ? WHERE id = ?`, t.TempDir(), int64(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id = 'conv-k', spec = '{}'`); err != nil {
		t.Fatal(err)
	}
	return f, id
}

// A removed thread's Send is refused before its hooks would run, as a closed
// one's is.
func TestSendToARemovedThreadIsRefusedBeforeItsHooks(t *testing.T) {
	ctx := context.Background()
	f, id := closedThread(t)
	hooks := &countHooks{}
	f.core.Hooks = hooks
	owner := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | reopener})
	set := func(state control.State) {
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET state = ? WHERE thread_id = ?`, string(state), int64(id)); err != nil {
			t.Error(err)
		}
	}
	set(control.Removed)
	if f.threadState(t, id) != control.Removed {
		t.Fatalf("the thread is %s", f.threadState(t, id))
	}
	f.core.stateRead = func() { set(control.Idle) }
	if err := owner.Send(ctx, id, "m1", control.Input{Text: "wake up"}, control.Deliver); !errors.Is(err, ErrClosed) || hooks.n.Load() != 0 {
		t.Fatalf("the send: %v, %d hook calls, want ErrClosed and none", err, hooks.n.Load())
	}
}
