package supervisor

import (
	"context"
	"fmt"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

func TestWorkerEndsHaveIndependentHomeReaders(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	beside(t, c, control.Idle, control.Waiting, control.Asleep)
	exec(t, c, `INSERT INTO thread_homes (thread, after, home) VALUES (2, 10, 1), (2, 30, 5)`)
	readers := []*waking{}
	for _, home := range []control.ThreadID{0, older, newer} {
		readers = append(readers, newWaking(&Waker{Core: c, Scope: "c1", target: home, Clock: at(besideNow), Log: c.Log}))
	}
	for _, e := range []control.Event{turnEnd(9, worker), turnEnd(20, worker), turnEnd(40, worker)} {
		for _, r := range readers {
			if err := r.event(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, want := range []uint64{40, 20, 0} {
		if got := readers[i].ended[worker].Pos; got != want {
			t.Fatalf("reader %d: end %d, want %d", i, got, want)
		}
	}
	if id, ready, err := readers[1].ready(ctx); err != nil || id != older || ready {
		t.Fatalf("blocked home: %d, %t, %v", id, ready, err)
	}
	if id, ready, err := readers[0].ready(ctx); err != nil || id != own || !ready {
		t.Fatalf("own beside blocked home: %d, %t, %v", id, ready, err)
	}
	if id, ready, err := readers[2].ready(ctx); err != nil || id != newer || !ready {
		t.Fatalf("asleep beside blocked home: %d, %t, %v", id, ready, err)
	}
	for _, state := range []control.State{control.Closed, control.Failed, control.Removed} {
		exec(t, c, fmt.Sprintf(`UPDATE threads SET state = '%s' WHERE id = 1`, state))
		if id, ready, err := readers[1].ready(ctx); err != nil || id != own || !ready {
			t.Fatalf("home %s: %d, %t, %v", state, id, ready, err)
		}
	}
}

func TestConversationWakeStorageAndKeysAreIndependent(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	beside(t, c, control.Idle, control.Idle, control.Idle)
	if err := Begin(ctx, c, "c1"); err != nil {
		t.Fatal(err)
	}
	exec(t, c, `UPDATE supervisor_wakes SET pos = 10, since = 3, send_to = 20, send_key = 'hataori-wake-20-5', send_text = 'legacy' WHERE scope = 'c1'`)
	for _, home := range []control.ThreadID{older, newer} {
		r := newWaking(&Waker{Core: c, Scope: "c1", target: home})
		if err := r.load(ctx); err != nil {
			t.Fatal(err)
		}
		if r.pos != 3 || r.sending != nil {
			t.Fatalf("new reader inherited own pending wake: %+v", r)
		}
		r.pos = uint64(10 + home)
		r.sending = &wake{to: 40, key: fmt.Sprintf("%s~%d", WakeKey(own, 40), home), text: fmt.Sprint(home)}
		if err := r.store(ctx); err != nil {
			t.Fatal(err)
		}
		fresh := newWaking(r.w)
		if err := fresh.load(ctx); err != nil {
			t.Fatal(err)
		}
		if fresh.pos != r.pos || fresh.sending.key != r.sending.key || fresh.sending.text != r.sending.text {
			t.Fatalf("reader %d lost durable state", home)
		}
		key, err := keyFor(fresh.sending.key, 15, nil)
		if want := fmt.Sprintf("%s~%d", WakeKey(15, 40), home); err != nil || key != want {
			t.Fatalf("retargeted key %q, %v; want %q", key, err, want)
		}
	}
	ownReader := newWaking(&Waker{Core: c, Scope: "c1"})
	if err := ownReader.load(ctx); err != nil {
		t.Fatal(err)
	}
	if ownReader.pos != 10 || ownReader.sending.text != "legacy" {
		t.Fatal("conversation storage changed the legacy pending wake")
	}
}

func TestOneHomeCannotAcknowledgeAnotherHomesEnd(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	beside(t, c, control.Idle, control.Waiting, control.Idle)
	exec(t, c, `INSERT INTO supervisor_asked (scope, key, asks_in, person, thread, wake, woken, woken_home) VALUES ('c1', 'work', 1, 'ann', 2, 'other-wake', 40, 8)`,
		`INSERT INTO supervisor_conversation_given (scope, home, thread, upto) VALUES ('c1', 8, 2, 40)`)
	for _, home := range []control.ThreadID{0, older, newer} {
		r := newWaking(&Waker{Core: c, Scope: "c1", target: home})
		got, err := r.wokenTo(ctx, worker)
		want := uint64(0)
		if home == newer {
			want = 40
		}
		if err != nil || got != want {
			t.Fatalf("reader %d acknowledged %d, %v; want %d", home, got, err, want)
		}
	}
}

func TestAcceptedHomeWakeTracksInputLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, state, first                 string
		launch                             int
		queued, started, fallback, waiting bool
	}{
		{name: "queued", state: "idle", queued: true, waiting: true},
		{name: "launch requested", state: "creating", first: `{"key":"wake"}`, launch: 1, waiting: true},
		{name: "process preparing", state: "creating", first: `{"key":"wake"}`, launch: 2, waiting: true},
		{name: "process awaits ready", state: "creating", first: `{"key":"wake"}`, waiting: true},
		{name: "closed queue", state: "closed", fallback: true},
		{name: "reopened after queue dropped", state: "asleep", first: `{"key":"wake"}`, fallback: true},
		{name: "failed launch", state: "failed", first: `{"key":"wake"}`, fallback: true},
		{name: "already started then closed", state: "closed", started: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := remindStore(t)
			beside(t, c, control.Asleep, control.State(tc.state), control.Idle)
			exec(t, c, fmt.Sprintf(`INSERT INTO sessions (id, thread_id, agent, state, first_input, launch, created_at, updated_at) VALUES (1, 1, 'fake', '%s', '%s', %d, 0, 0)`, tc.state, tc.first, tc.launch))
			if err := c.Store.Update(ctx, func(tx *store.Tx) error {
				_, err := tx.Accept(ctx, "send:c1\x00wake", []byte(`{"thread":1}`))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if tc.queued {
				exec(t, c, `INSERT INTO waiting_inputs (thread_id, input, wake, pos) VALUES (1, '{"key":"wake"}', 1, 0)`)
			}
			if tc.started {
				exec(t, c, `INSERT INTO events (scope, thread, thread_id, time, kind, data) VALUES ('c1', '', 1, 0, 'turn_started', '{"key":"wake","session":1,"turn":1,"by":"caller"}')`)
			}
			r := newWaking(&Waker{Core: c, Scope: "c1", target: older})
			r.sending = &wake{key: "wake"}
			delivered, waiting, err := r.accepted(ctx)
			if err != nil || delivered != tc.started || waiting != tc.waiting || r.sending.fallback != tc.fallback {
				t.Fatalf("delivered=%v waiting=%v fallback=%v err=%v", delivered, waiting, r.sending.fallback, err)
			}
			if tc.fallback {
				if id, _, err := r.destination(ctx); err != nil || id != own {
					t.Fatalf("fallback to %d, %v", id, err)
				}
			}
		})
	}
}
