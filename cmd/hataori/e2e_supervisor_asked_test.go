package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #677: the first turn end of a thread the
// supervisor started during a person's turn wakes it past a used-up hourly
// budget. End to end with the fake agent, whose supervisor calls
// start_thread as its script says, and the wake test's clock.

// startsThread is the fake supervisor's script that starts a thread with
// script.
func (wt *wakeTest) startsThread(script string) string {
	return fmt.Sprintf("mcp start_thread {\"repo\": %q, \"text\": %q}", wt.repo, script)
}

// newest is the newest thread that is no supervisor's, once it is not
// after.
func (wt *wakeTest) newest(t *testing.T, after control.ThreadID) control.ThreadID {
	t.Helper()
	var id int64
	eventually(t, "the thread the supervisor started", 15*time.Second, func() bool {
		wt.db.QueryRow(`SELECT coalesce(max(id), 0) FROM threads WHERE scope = 'c1' AND confined = 0`).Scan(&id)
		return id > int64(after)
	})
	return control.ThreadID(id)
}

// asks gives the supervisor a person's message that has it start a thread
// with script, and returns that thread and the end of its first turn.
func (wt *wakeTest) asks(t *testing.T, owner *wire.Client, sv control.ThreadInfo, key, script string) (control.ThreadID, uint64) {
	t.Helper()
	var before int64
	wt.db.QueryRow(`SELECT coalesce(max(id), 0) FROM threads`).Scan(&before)
	if a := sendAndRead(t, owner, sv.ID, key, wt.startsThread(script)); strings.HasPrefix(a, "mcp error") || strings.HasPrefix(a, "mcp failed") {
		t.Fatalf("start_thread: %s", a)
	}
	id := wt.newest(t, control.ThreadID(before))
	return id, wt.ended(t, id, 1)
}

// quietly waits until the quiet period's timer runs beside the hold's, and
// moves the clock past the quiet period, less than the hold's step.
func (wt *wakeTest) quietly(t *testing.T) {
	t.Helper()
	eventually(t, "the quiet period beside the hold", 10*time.Second, func() bool { return wt.clock().waiting() == 2 })
	wt.clock().advance(30 * time.Second)
}

// stays checks that sv keeps n wakes while the clock moves on by d.
func (wt *wakeTest) stays(t *testing.T, sv control.ThreadID, n int, d time.Duration, what string) {
	t.Helper()
	wt.steps(d)
	time.Sleep(300 * time.Millisecond)
	if got := wt.wakes(t, sv); len(got) != n {
		t.Fatalf("%s: %d wakes, want %d: %q", what, len(got), n, got)
	}
}

func names(digest string, id control.ThreadID) bool {
	return strings.Contains(digest, fmt.Sprintf("\nThread %d: ", id))
}

// With the hourly turns used up and another thread's turn end held, the
// first turn end of a thread started in a person's turn wakes the
// supervisor within the quiet period, alone, and counts as a turn; its
// second is held as any other. The held ones come in one wake at the next
// hour, across restarts, without the first again.
func TestAskedThreadWakesPastTheTurnLimit(t *testing.T) {
	wt := newWakeTest(t, budget(1, 0, 0), autonomy("act"))
	stop, owner, sv := wt.start(t) // its first turn is the hour's
	x, pos := wt.worker(t, owner, "c1", "x", "reply held")
	wt.held(t, pos)
	w, pos := wt.asks(t, owner, sv, "q1", "reply the answer")
	wt.read(t, pos)
	turns := wt.turns(t, sv.ID)
	wt.quietly(t)
	got := wt.woken(t, owner, sv.ID, 1)
	if !names(got[0], w) || names(got[0], x) || !strings.Contains(got[0], "> the answer") {
		t.Fatalf("the wake past the limit:\n%s", got[0])
	}
	if n := wt.turns(t, sv.ID); n != turns+1 {
		t.Fatalf("%d supervisor turns, want %d", n, turns+1)
	}
	stop()

	stop, owner, sv = wt.start(t)
	wt.stays(t, sv.ID, 1, 2*time.Minute, "after a restart")
	wt.again(t, owner, w, "w-2", "reply more", 2)
	wt.stays(t, sv.ID, 1, 5*time.Minute, "after the second turn end of the thread")
	stop()

	stop, owner, sv = wt.start(t)
	defer stop()
	wt.stays(t, sv.ID, 1, 2*time.Minute, "after another restart")
	wt.until(t, wakeBase.Add(time.Hour))
	got = wt.woken(t, owner, sv.ID, 2)
	if !names(got[1], x) || !names(got[1], w) || !strings.Contains(got[1], "> more") || strings.Contains(got[1], "> the answer") {
		t.Fatalf("the wake of the next hour:\n%s", got[1])
	}
	wt.stays(t, sv.ID, 2, 20*time.Minute, "after it")
}

// A thread started in a wake's turn, which no person started, is held.
func TestThreadStartedInAWakeIsHeld(t *testing.T) {
	wt := newWakeTest(t, budget(2, 0, 0), autonomy("act"), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	a, pos := wt.worker(t, owner, "c1", "a", "reply "+wt.startsThread("reply nobody asked"))
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1) // the second turn of the hour: it starts a thread
	w := wt.newest(t, a)
	wt.held(t, wt.ended(t, w, 1))
	wt.stays(t, sv.ID, 1, 10*time.Minute, "a thread started in a wake's turn")
	wt.until(t, wakeBase.Add(time.Hour))
	if got := wt.woken(t, owner, sv.ID, 2); !names(got[1], w) {
		t.Fatalf("the wake of the next hour:\n%s", got[1])
	}
}

// The day's tokens hold a marked thread too, also with the turns used up.
func TestAskedThreadIsHeldByTheTokens(t *testing.T) {
	wt := newWakeTest(t, budget(1, 0, 1000), autonomy("act"))
	stop, owner, sv := wt.start(t)
	defer stop()
	sendAndRead(t, owner, sv.ID, "u", "usage 1300 0 0 0 0 0\nreply spent")
	w, pos := wt.asks(t, owner, sv, "q1", "reply the answer")
	wt.held(t, pos)
	// Past the hour, unless the day ends first.
	next := time.Date(wakeBase.Year(), wakeBase.Month(), wakeBase.Day()+1, 0, 0, 0, 0, time.Local)
	wait := 90 * time.Minute
	if left := next.Sub(wt.clock().Now()); wait > left-10*time.Minute {
		wait = 20 * time.Minute
	}
	wt.stays(t, sv.ID, 0, wait, "with the tokens used up")
	wt.until(t, next)
	if got := wt.woken(t, owner, sv.ID, 1); !names(got[0], w) {
		t.Fatalf("the wake of the next day:\n%s", got[0])
	}
}

// The hour's sends do not hold it either.
func TestAskedThreadWakesPastTheSendLimit(t *testing.T) {
	wt := newWakeTest(t, budget(0, 1, 0), autonomy("act"))
	stop, owner, sv := wt.start(t)
	defer stop()
	w, pos := wt.asks(t, owner, sv, "q1", "reply the answer") // the hour's send
	wt.read(t, pos)
	wt.quiet(t)
	if got := wt.woken(t, owner, sv.ID, 1); !names(got[0], w) {
		t.Fatalf("the wake:\n%s", got[0])
	}
}

// Under propose the start runs when a person approves it, after the
// person's turn: its thread is marked all the same. The decision waits for
// the next hour.
func TestAskedThreadOfAnApprovedProposal(t *testing.T) {
	wt := newWakeTest(t, budget(1, 0, 0), autonomy("propose"))
	stop, owner, sv := wt.start(t)
	defer stop()
	if a := sendAndRead(t, owner, sv.ID, "q1", wt.startsThread("reply the answer")); !strings.Contains(a, "roposal") {
		t.Fatalf("start_thread under propose: %s", a)
	}
	f := &svAutonomy{wt: wt, owner: owner, sv: sv}
	f.conv.Channel, f.conv.Thread = sv.Links[0].Channel, sv.Links[0].Thread
	f.press(t, f.shown(t, 1), e2eHook, "Approve")
	w := wt.newest(t, 0)
	wt.read(t, wt.ended(t, w, 1))
	wt.settle(t, "the wake", func() bool { return len(wt.wakes(t, sv.ID)) >= 1 })
	got := wt.woken(t, owner, sv.ID, 1)
	if !names(got[0], w) || strings.Contains(got[0], "Decided by a person") {
		t.Fatalf("the wake past the limit:\n%s", got[0])
	}
	wt.until(t, wakeBase.Add(time.Hour))
	if got := wt.woken(t, owner, sv.ID, 2); !strings.Contains(got[1], "Your proposal 1, to start a thread: approved by") {
		t.Fatalf("the wake of the next hour:\n%s", got[1])
	}
}

// A daemon that died after the wake past the limit was accepted, or before
// it was sent, gives it once, and keeps what is held.
func TestAskedThreadWakesOnceAcrossARestart(t *testing.T) {
	for _, step := range []string{"stored", "sent"} {
		t.Run(step, func(t *testing.T) {
			wt := newWakeTest(t, budget(1, 0, 0), autonomy("act"))
			stopped := errors.New("stopped here")
			h := func(at string) error {
				if at == step {
					return stopped
				}
				return nil
			}
			wt.hook.Store(&h)
			stop, owner, sv := wt.start(t)
			x, pos := wt.worker(t, owner, "c1", "x", "reply held")
			wt.held(t, pos)
			w, pos := wt.asks(t, owner, sv, "q1", "reply the answer")
			wt.read(t, pos)
			wt.quietly(t)
			eventually(t, "the wake stored", 15*time.Second, func() bool {
				var n int
				wt.db.QueryRow(`SELECT count(*) FROM supervisor_wakes WHERE send_key IS NOT NULL`).Scan(&n)
				return n == 1
			})
			if step == "sent" {
				wt.woken(t, owner, sv.ID, 1)
			}
			stop()
			stop, owner, sv = wt.start(t)
			defer stop()
			if got := wt.woken(t, owner, sv.ID, 1); !names(got[0], w) || names(got[0], x) {
				t.Fatalf("the wake past the limit:\n%s", got[0])
			}
			wt.stays(t, sv.ID, 1, 5*time.Minute, "after the restart")
			wt.until(t, wakeBase.Add(time.Hour))
			if got := wt.woken(t, owner, sv.ID, 2); !names(got[1], x) || names(got[1], w) {
				t.Fatalf("the wake of the next hour:\n%s", got[1])
			}
		})
	}
}

// A wake past the limit tells no other thread's turn end: the loop guard
// counts no cycle for one that is held.
func TestAskedWakeIsNoCycleOfAHeldThread(t *testing.T) {
	wt := newWakeTest(t, budget(1, 0, 0), autonomy("act"))
	stop, owner, sv := wt.start(t)
	defer stop()
	x, pos := wt.worker(t, owner, "c1", "x", "reply held")
	wt.held(t, pos)
	// In one turn of a person, so that nothing resets the count in between:
	// a send to x, whose turn end is held, and a start.
	sendAndRead(t, owner, sv.ID, "q1", fmt.Sprintf("mcp send_to_thread {\"number\": %d, \"text\": \"reply sent\"}\n", x)+wt.startsThread("reply the answer"))
	wt.read(t, wt.ended(t, x, 2))
	w := wt.newest(t, x)
	wt.read(t, wt.ended(t, w, 1))
	wt.quietly(t)
	if got := wt.woken(t, owner, sv.ID, 1); !names(got[0], w) || names(got[0], x) {
		t.Fatalf("the wake past the limit:\n%s", got[0])
	}
	wt.mu.Lock()
	g := &supervisor.LoopGuard{Core: wt.waker.Core, Scope: "c1", Cycles: 3}
	wt.mu.Unlock()
	if n, _, err := g.Count(context.Background(), x); err != nil || n != 0 {
		t.Fatalf("%d cycles of the held thread, %v; want 0", n, err)
	}
	if n, _, err := g.Count(context.Background(), w); err != nil || n != 1 {
		t.Fatalf("%d cycles of the thread that woke it, %v; want 1", n, err)
	}
}

// More marked threads than a digest shows come in more wakes, each shown.
func TestAskedThreadsOverThreadsMax(t *testing.T) {
	wt := newWakeTest(t, budget(1, 0, 0), autonomy("act"), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["threads_max"] = 1
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	sendAndRead(t, owner, sv.ID, "q1", wt.startsThread("reply one answer")+"\n"+wt.startsThread("reply another answer"))
	b := wt.newest(t, 2)
	a := b - 1
	wt.ended(t, a, 1)
	wt.read(t, max(wt.ended(t, a, 1), wt.ended(t, b, 1)))
	wt.settle(t, "two wakes", func() bool { return len(wt.wakes(t, sv.ID)) >= 2 })
	got := wt.woken(t, owner, sv.ID, 2)
	if !names(got[0], a) || names(got[0], b) || !names(got[1], b) || names(got[1], a) {
		t.Fatalf("the wakes:\n%s\n%s", got[0], got[1])
	}
}

// A mark left by a daemon that died before its start made anything does
// not pass to a start under the same key in a wake's turn, or in none.
func TestAskedMarkOfALostStartGoes(t *testing.T) {
	var (
		mu    sync.Mutex
		issue func(string) (string, error)
	)
	supervisorHook = func(i func(string) (string, error), _ func(string)) { mu.Lock(); issue = i; mu.Unlock() }
	t.Cleanup(func() { supervisorHook = nil })
	wt := newWakeTest(t, budget(1, 0, 0), autonomy("act"))
	stop, _, sv := wt.start(t)
	defer stop()
	wt.mu.Lock()
	st := wt.waker.Core.Store
	wt.mu.Unlock()
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO supervisor_asked (scope, key, asks_in, person) VALUES ('c1', 'lost', 0, 'boss')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	svc := svConn(t, wt.r.root, issue)
	mu.Unlock()
	w, err := svc.Start(context.Background(), control.StartRequest{Key: "lost", Scope: "c1", Repo: wt.repo, Input: control.Input{Text: "reply nobody asked"}})
	if err != nil {
		t.Fatal(err)
	}
	wt.held(t, wt.ended(t, w, 1))
	wt.stays(t, sv.ID, 0, 5*time.Minute, "after a start in no person's turn")
}

// A wake that is not held carries the mark of a thread it only counts too:
// that was its first wake, and its next turn end is held as any.
func TestAskedMarkOfAThreadOnlyCounted(t *testing.T) {
	wt := newWakeTest(t, budget(3, 0, 0), autonomy("act"), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["threads_max"] = 1
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	sendAndRead(t, owner, sv.ID, "q1", wt.startsThread("reply one answer")+"\n"+wt.startsThread("reply another answer")) // the second turn
	b := wt.newest(t, 2)
	a := b - 1
	wt.read(t, max(wt.ended(t, a, 1), wt.ended(t, b, 1)))
	wt.quiet(t)
	if d := wt.woken(t, owner, sv.ID, 1)[0]; !names(d, a) || names(d, b) || !strings.Contains(d, "And 1 more thread") {
		t.Fatalf("the wake, the third turn:\n%s", d)
	}
	wt.again(t, owner, b, "b-2", "reply more", 2)
	wt.stays(t, sv.ID, 1, 10*time.Minute, "after the second turn end of the thread only counted")
}
