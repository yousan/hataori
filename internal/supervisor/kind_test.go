package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
)

// The threads of scope c1 of a beside: the supervisor's own one, a
// conversation older and one newer than it, a look-up newer than both, and
// an ordinary one.
const (
	older, worker, own, newer, lookUp = 1, 2, 5, 8, 9
)

// besideNow is when the events of a beside happen, and its clock stands.
var besideNow = time.Date(2026, 1, 2, 3, 30, 0, 0, time.UTC)

func exec(t *testing.T, c *core.Core, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := c.Store.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// beside puts the threads into c's store, in these states; "" for none.
func beside(t *testing.T, c *core.Core, ownState, olderState, newerState control.State) {
	t.Helper()
	for _, th := range []struct {
		id    int
		kind  control.ThreadKind
		state control.State
	}{
		{older, control.KindConversation, olderState}, {worker, control.KindOrdinary, control.Idle}, {own, control.KindSupervisor, ownState},
		{newer, control.KindConversation, newerState}, {lookUp, control.KindLookup, newerState},
	} {
		if th.state == "" {
			continue
		}
		exec(t, c, fmt.Sprintf(`INSERT INTO threads (id, scope, key, started_by, state, confined, kind, workspace, created_at, updated_at)
			VALUES (%d, 'c1', 'k%d', 'p', '%s', %t, '%s', '/root/confined/%d', 0, 0)`, th.id, th.id, th.state, th.kind.Confined(), th.kind, th.id))
	}
}

// arrangements runs f with conversations older and newer than the own
// thread, each of them open, closed or not there, and the own one in state.
func arrangements(t *testing.T, state control.State, f func(t *testing.T, put func(*core.Core))) {
	for _, o := range []control.State{"", control.Idle, control.Closed} {
		for _, n := range []control.State{"", control.Idle, control.Closed} {
			t.Run(fmt.Sprintf("own %s, older %q, newer %q", state, o, n), func(t *testing.T) {
				f(t, func(c *core.Core) { beside(t, c, state, o, n) })
			})
		}
	}
}

// What the daemon's start finds is the supervisor's own thread and its
// directory, whatever conversations and look-ups there are beside it.
func TestStartFindsTheOwnThread(t *testing.T) {
	arrangements(t, control.Asleep, func(t *testing.T, put func(*core.Core)) {
		s, _ := layerSessions(t, &bytes.Buffer{})
		put(s.Core)
		if id, err := s.StartOne(context.Background(), "c1"); err != nil || id != own {
			t.Errorf("started %d, %v; want thread %d, which it has", id, err, own)
		}
		if dir, err := s.Dir(context.Background(), "c1"); err != nil || dir != "/root/confined/5" {
			t.Errorf("the scope's directory is %s, %v; want thread %d's", dir, err, own)
		}
	})
}

// A notice of the budget and one of the loop guard is told in the
// supervisor's own thread, also in a closed one.
func TestNoticesGoToTheOwnThread(t *testing.T) {
	for _, state := range []control.State{control.Idle, control.Closed} {
		arrangements(t, state, func(t *testing.T, put func(*core.Core)) {
			ctx := context.Background()
			c := remindStore(t)
			put(c)
			var told []control.ThreadID
			tell := func(_ context.Context, id control.ThreadID, key, text string) error {
				told = append(told, id)
				return nil
			}
			exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data)
				VALUES (10, 'c1', '', %d, %d, 'turn_started', '{"by":"caller","key":"k","session":1,"turn":5}')`, own, besideNow.UnixMilli()))
			b := &Budget{Core: c, Scope: "c1", Clock: at(besideNow), Zone: time.UTC, Limits: Limits{Turns: 1}, Tell: tell, Log: c.Log}
			if _, until, err := b.Admit(ctx, "c1", []Ended{{Thread: worker}}); err != nil || until.IsZero() {
				t.Fatalf("the budget held nothing: %v, %v", until, err)
			}
			g := &LoopGuard{Core: c, Scope: "c1", Cycles: 3, Tell: tell, Log: c.Log}
			g.tell(ctx, worker, 3, 0)
			if fmt.Sprint(told) != fmt.Sprint([]control.ThreadID{own, own}) {
				t.Errorf("told in threads %v, want twice in %d", told, own)
			}
		})
	}
}

// The waker finds the supervisor's own thread: at its start, at a worker's
// turn end, when a wake is ready to be given and when it is put to sleep
// for it.
func TestWakerFindsTheOwnThread(t *testing.T) {
	arrangements(t, control.Idle, func(t *testing.T, put func(*core.Core)) {
		ctx := context.Background()
		c := remindStore(t)
		put(c)
		var slept []control.ThreadID
		r := newWaking(&Waker{Core: c, Scope: "c1", Log: c.Log, Clock: at(besideNow),
			Sleep: func(_ context.Context, id control.ThreadID) (bool, error) {
				slept = append(slept, id)
				return true, nil
			}})
		if err := r.event(ctx, turnEnd(20, worker)); err != nil {
			t.Fatal(err)
		}
		if r.sv != own || len(r.ended) != 1 {
			t.Errorf("at thread %d's turn end: the supervisor's is %d, folded %v; want %d, and the one", worker, r.sv, r.ended, own)
		}
		r.sv = 0
		if id, ready, err := r.ready(ctx); err != nil || id != own || !ready {
			t.Errorf("ready: thread %d, %v, %v; want %d", id, ready, err, own)
		}
		if ok, err := r.toSleep(ctx, own); err != nil || !ok || fmt.Sprint(slept) != fmt.Sprint([]control.ThreadID{own}) {
			t.Errorf("put to sleep: %v, %v, %v; want %d", slept, ok, err, own)
		}

		// At its start: a wake that could not start the own thread's agent is
		// the first event it reads.
		if err := Begin(ctx, c, "c1"); err != nil {
			t.Fatal(err)
		}
		exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data)
			VALUES (30, 'c1', '', %d, 0, 'state', '{"from":"creating","to":"asleep","reason":"no"}')`, own))
		told := make(chan control.ThreadID, 9)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		w := &Waker{Core: c, Scope: "c1", Owner: "owner", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: at(besideNow),
			Tell: func(_ context.Context, id control.ThreadID, key, text string) error {
				told <- id
				cancel()
				return nil
			}}
		w.run(ctx)
		select {
		case id := <-told:
			if id != own {
				t.Errorf("at the waker's start the supervisor's thread is %d, want %d", id, own)
			}
		default:
			t.Error("at its start the waker did not take the own thread for the supervisor's")
		}
	})
}

func newWaking(w *Waker) *waking {
	return &waking{w: w, ended: map[control.ThreadID]control.Event{}, before: map[control.ThreadID]control.Event{},
		kinds: map[control.ThreadID]control.ThreadKind{}, waiting: map[control.ThreadID]control.Event{},
		stale: map[control.ThreadID]control.Event{}, asks: map[control.ThreadID]bool{}}
}

func turnEnd(pos uint64, id control.ThreadID) control.Event {
	return control.Event{Pos: pos, Scope: "c1", Thread: id, Kind: TurnEnded,
		Data: []byte(`{"by":"caller","from":"chat:ann","outcome":"completion","background":0,"session":3,"turn":7}`)}
}

// A conversation's and a look-up's turn end, failed start and background
// wait wake nothing, and neither does a decision in one; a decision in the
// own thread does.
func TestConversationsWakeNothing(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	beside(t, c, control.Idle, control.Idle, control.Failed)
	r := newWaking(&Waker{Core: c, Scope: "c1", Log: c.Log, Clock: at(besideNow)})
	decided := func(pos uint64, id control.ThreadID, kind control.EventKind) control.Event {
		return control.Event{Pos: pos, Scope: "c1", Thread: id, Kind: kind, Data: []byte(`{"outcome":"approved"}`)}
	}
	// As if each failed to start and the supervisor's id had started it.
	for i, id := range []int{older, worker, newer, lookUp} {
		exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES (%d, 'c1', '', %d, 0, 'thread_created',
			'{"from":"supervisor:c1","principal":"sv","repo":"","session":3,"thread":%d,"model":"","effort":""}')`, 10+i, id, id),
			fmt.Sprintf(`UPDATE threads SET state = 'failed' WHERE id = %d`, id))
	}
	failed := func(pos uint64, id control.ThreadID) control.Event {
		return control.Event{Pos: pos, Scope: "c1", Thread: id, Kind: "state", Data: []byte(`{"from":"creating","to":"failed","reason":"no"}`)}
	}
	for i, id := range []control.ThreadID{older, newer, lookUp} {
		pos := uint64(10 * (i + 2))
		waits := turnEnd(pos+1, id)
		waits.Data = bytes.Replace(waits.Data, []byte(`"background":0`), []byte(`"background":1`), 1)
		for _, e := range []control.Event{turnEnd(pos, id), waits,
			failed(pos+2, id),
			decided(pos+3, id, ApprovalAnswered), decided(pos+4, id, ProposalAnswered)} {
			if err := r.event(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !r.empty() || len(r.waiting) != 0 || len(r.stale) != 0 || r.quiet != nil {
		t.Fatalf("folded %v, decided %v, waiting %v", r.ended, r.decided, r.waiting)
	}
	if err := r.event(ctx, decided(60, own, ProposalAnswered)); err != nil || len(r.decided) != 1 {
		t.Fatalf("a decision in the own thread: %v, %v", r.decided, err)
	}
	if err := r.event(ctx, failed(61, worker)); err != nil || len(r.ended) != 1 {
		t.Fatalf("a worker's failed start: %v, %v", r.ended, err)
	}

	// Found again at a start: neither waits for its background tasks.
	for i, id := range []int{older, worker, newer} {
		exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES (%d, 'c1', '', %d, %d, 'turn_ended',
			'{"by":"caller","from":"chat:ann","outcome":"completion","background":1,"session":3,"turn":7}')`, 70+i, id, besideNow.UnixMilli()))
	}
	if err := r.load(ctx); err != nil {
		t.Fatal(err)
	}
	exec(t, c, `UPDATE supervisor_wakes SET since = 0 WHERE scope = 'c1'`)
	r.pos = 80
	if err := r.loadWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.waiting) != 1 || r.waiting[worker].Pos != 71 {
		t.Fatalf("waiting at a start: %v, want thread %d's", r.waiting, worker)
	}
	// And an input of a conversation that reads like a wake is none that told
	// of it.
	stale := fmt.Sprintf("hataori: a digest.\n\nThread %d: w.\n%s\n", worker, staleLine)
	for i, id := range []int{newer, own} {
		if told, err := r.toldStale(ctx, r.waiting[worker]); err != nil || told {
			t.Fatalf("told before any wake did: %v, %v", told, err)
		}
		exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES (%d, 'c1', '', %d, 0, 'input',
			'{"from":"","key":"k","text":%q}')`, 90+i, id, stale))
	}
	if told, err := r.toldStale(ctx, r.waiting[worker]); err != nil || !told {
		t.Fatalf("told by a wake of the own thread: %v, %v", told, err)
	}
}

// The turns of the own thread count in the budget, whoever started them,
// and a conversation's and a look-up's do not; the tokens of all of them
// do, and an ordinary thread's neither.
func TestBudgetOfConversations(t *testing.T) {
	c := remindStore(t)
	beside(t, c, control.Idle, control.Closed, control.Idle)
	for i, id := range []int{older, worker, own, newer, lookUp} {
		exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
			(%d, 'c1', '', %d, %d, 'turn_started', '{"by":"caller","key":"k","session":%d,"turn":%d}'),
			(%d, 'c1', '', %d, %d, 'usage', '{"input":%d,"output":0,"cache_read":0,"cache_write":0,"session":%d,"turn":%d}')`,
			10+2*i, id, besideNow.UnixMilli(), id, id, 11+2*i, id, besideNow.UnixMilli(), 1<<i, id, id))
	}
	b := &Budget{Core: c, Scope: "c1", Clock: at(besideNow), Zone: time.UTC, Log: c.Log}
	u, err := b.Used(context.Background())
	if err != nil || u.Turns != 1 || u.Tokens != 1+4+8+16 {
		t.Fatalf("used %d turns and %d tokens, %v; want 1 and 29", u.Turns, u.Tokens, err)
	}
}

// What a person does in the own thread resets the loop guard's count, and
// what one does in a conversation or a look-up does not; only the own
// thread's wakes are cycles.
func TestLoopGuardCountsInTheOwnThread(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		woken, spoke int // the thread woken, and the one a person spoke in after it, if any
		want         int
	}{
		"woken":                             {own, 0, 1},
		"a conversation woken":              {newer, 0, 0},
		"a look-up woken":                   {lookUp, 0, 0},
		"a person in the own thread":        {own, own, 0},
		"a person in an older conversation": {own, older, 1},
		"a person in a newer conversation":  {own, newer, 1},
		"a person in a look-up":             {own, lookUp, 1},
	} {
		t.Run(name, func(t *testing.T) {
			c := remindStore(t)
			beside(t, c, control.Idle, control.Idle, control.Idle)
			exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
				(10, 'c1', '', %d, 0, 'turn_started', '{"by":"caller","from":"supervisor:c1","key":"s","session":2,"turn":5}'),
				(11, 'c1', '', %d, 0, 'turn_ended', '{"by":"caller","from":"supervisor:c1","outcome":"completion","background":0,"session":2,"turn":5}'),
				(12, 'c1', '', %d, 0, 'turn_started', '{"by":"caller","key":"%s","session":1,"turn":6}')`,
				worker, worker, tc.woken, WakeKey(control.ThreadID(tc.woken), 11)))
			if tc.spoke != 0 {
				exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
					(13, 'c1', '', %d, 0, 'input', '{"from":"chat:ann","key":"p","text":"hello"}')`, tc.spoke))
			}
			g := &LoopGuard{Core: c, Scope: "c1", Cycles: 3, Log: c.Log}
			if n, _, err := g.Count(ctx, worker); err != nil || n != tc.want {
				t.Errorf("%d cycles, %v; want %d", n, err, tc.want)
			}
		})
	}
}

// The supervisor thread that started a thread is never a conversation or a
// look-up, though one is open and newer.
func TestStartedByIsTheOwnThread(t *testing.T) {
	for _, state := range []control.State{control.Idle, control.Closed} {
		arrangements(t, state, func(t *testing.T, put func(*core.Core)) {
			c := remindStore(t)
			put(c)
			for i, id := range []int{older, own, newer, lookUp} {
				exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data)
					SELECT %d, 'c1', '', id, 0, 'thread_created', '{}' FROM threads WHERE id = %d`, 10+2*i, id),
					fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data)
					SELECT %d, 'c1', '', id, 0, 'state', '{"from":"creating","to":"idle"}' FROM threads WHERE id = %d`, 11+2*i, id))
			}
			if state == control.Closed {
				exec(t, c, fmt.Sprintf(`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data)
					VALUES (20, 'c1', '', %d, 0, 'state', '{"from":"idle","to":"closed"}')`, own))
			}
			if got, err := StartedBy(context.Background(), c.Store.DB, "c1", ID("c1"), 30); err != nil || got != own {
				t.Errorf("started by %d, %v; want %d", got, err, own)
			}
		})
	}
}

// A person's input in a conversation is given no line of the wakes
// answered, as one in the own thread is.
func TestAnsweredIsTheOwnThreads(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	beside(t, c, control.Idle, control.Idle, control.Idle)
	for _, id := range []int{older, own, newer, lookUp} {
		exec(t, c,
			fmt.Sprintf(`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (%d, %d, 'fake', 'asleep', 0, 0)`, id, id),
			fmt.Sprintf(`INSERT INTO turns (id, session_id, started_by, trigger, started_at, aside) VALUES (%d, %d, 'caller', '%s', 0, 1)`,
				id, id, WakeKey(control.ThreadID(id), 11)))
	}
	for _, id := range []control.ThreadID{older, own, newer, lookUp} {
		err := c.Store.Update(ctx, func(tx *store.Tx) error {
			line, err := Answered(ctx, tx, id)
			if err == nil && (line != "") != (id == own) {
				t.Errorf("thread %d: %q", id, line)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
