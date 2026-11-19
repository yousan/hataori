package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// wakeClock is the clock of a test's daemons that the test moves forward:
// their wakers' and budgets' time, and the time of their events. It
// starts at wakeBase and goes on across restarts.
type wakeClock struct {
	mu     sync.Mutex
	now    time.Duration // what the timers go by
	slept  time.Duration // what the wall clock is ahead of them, as after the host slept
	timers []wakeTimer
}

// wakeBase is when a wakeClock starts: on the hour, so that a test knows
// its windows, and the next one, so that its events are never older than
// the real time, which the daemon's idle sleep goes by.
var wakeBase = func() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, time.Local)
}()

func (c *wakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wakeBase.Add(c.now + c.slept)
}

// sleep moves the wall clock forward by d and fires no timer, as a host
// that slept: a timer does not count the time asleep.
func (c *wakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept += d
}

// reset drops the timers, those of a waker that stopped.
func (c *wakeClock) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = nil
}

type wakeTimer struct {
	at time.Duration
	c  chan time.Time
}

func (c *wakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, wakeTimer{c.now + d, ch})
	return ch
}

// advance moves the clock forward by d and fires the timers that are due.
func (c *wakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.at <= c.now {
			t.c <- time.Time{}
		} else {
			kept = append(kept, t)
		}
	}
	c.timers = kept
}

// waiting counts the timers not fired yet.
func (c *wakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// wakeTest is a root whose scope c1 has a supervisor, and whose daemons
// run on a clock of the test's own.
type wakeTest struct {
	r     *chatRoot
	db    *sql.DB
	repo  string
	mu    sync.Mutex
	clk   *wakeClock
	waker *supervisor.Waker
	hook  atomic.Pointer[func(step string) error] // the next waker's Hook
	// before, when set, runs before the next waker does, in the daemon's
	// start.
	before atomic.Pointer[func()]
}

func newWakeTest(t *testing.T, more ...func(cfg map[string]any)) *wakeTest {
	t.Helper()
	r, _ := svRoot(t, true, more...)
	wt := &wakeTest{r: r, repo: bareRepo(t), clk: &wakeClock{}}
	coreHook = func(c *core.Core) { c.Machine.Now = wt.clk.Now }
	wakerHook = func(w *supervisor.Waker) {
		if b := wt.before.Swap(nil); b != nil {
			(*b)()
		}
		wt.mu.Lock()
		defer wt.mu.Unlock()
		if w.Scope != "c1" {
			t.Errorf("a waker for scope %q, which names no supervisor", w.Scope)
		}
		if len(more) == 0 {
			if w.Quiet != 30*time.Second || w.OutputMax != 1024 || w.ThreadsMax != 20 {
				t.Errorf("waker defaults: quiet %v, output %d, threads %d; want 30s, 1024, 20", w.Quiet, w.OutputMax, w.ThreadsMax)
			}
			want := supervisor.Limits{Turns: 12, Sends: 20, Tokens: 2_000_000}
			if len(w.Gates) != 2 || w.Gates[0].(*supervisor.LoopGuard).Cycles != 3 || w.Gates[1].(*supervisor.Budget).Limits != want {
				t.Errorf("the waker's gates %v; want the loop guard at 3 cycles, then the budget with the defaults %+v", w.Gates, want)
			}
		}
		wt.clk.reset()
		w.Clock = wt.clk
		if h := wt.hook.Swap(nil); h != nil {
			w.Hook = *h
		}
		wt.waker = w
	}
	t.Cleanup(func() { wakerHook, coreHook = nil, nil })
	return wt
}

// start runs the daemon and returns its stop, a client of the owner and
// the supervisor's thread, idle after its first turn.
func (wt *wakeTest) start(t *testing.T) (func(), *wire.Client, control.ThreadInfo) {
	t.Helper()
	stop := runDaemon(t, wt.r.root)
	owner, err := wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	if wt.db == nil {
		wt.db = wt.r.db(t)
	}
	return stop, owner, supervisorThread(t, owner, "c1", control.Idle)
}

func (wt *wakeTest) clock() *wakeClock {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	return wt.clk
}

// worker starts a thread in scope with script and waits until its turn
// ended; it returns the thread and the position of that turn end.
func (wt *wakeTest) worker(t *testing.T, owner *wire.Client, scope, key, script string) (control.ThreadID, uint64) {
	t.Helper()
	id, err := owner.Start(context.Background(), control.StartRequest{Key: key, Scope: scope, Thread: "t-" + key, Repo: wt.repo,
		Input: control.Input{Text: script}})
	if err != nil {
		t.Fatal(err)
	}
	return id, wt.ended(t, id, 1)
}

// ended waits until thread id has n turn ends, and returns the position
// of the last.
func (wt *wakeTest) ended(t *testing.T, id control.ThreadID, n int) uint64 {
	t.Helper()
	var pos uint64
	eventually(t, fmt.Sprintf("turn end %d of thread %d", n, id), 20*time.Second, func() bool {
		var count int
		err := wt.db.QueryRow(`SELECT count(*), coalesce(max(pos), 0) FROM events WHERE thread_id = ? AND kind = 'turn_ended'`,
			int64(id)).Scan(&count, &pos)
		return err == nil && count >= n
	})
	return pos
}

// read waits until the waker has read the events up to pos.
func (wt *wakeTest) read(t *testing.T, pos uint64) {
	t.Helper()
	eventually(t, fmt.Sprintf("the waker to read up to %d", pos), 10*time.Second, func() bool {
		wt.mu.Lock()
		w := wt.waker
		wt.mu.Unlock()
		return w != nil && w.Read() >= pos
	})
}

// inputs returns the texts of the inputs of thread sv.
func (wt *wakeTest) inputs(t *testing.T, sv control.ThreadID) []string {
	t.Helper()
	rows, err := wt.db.Query(`SELECT data FROM events WHERE thread_id = ? AND kind = 'input' ORDER BY pos`, int64(sv))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var data []byte
		rows.Scan(&data)
		var in struct{ Text string }
		json.Unmarshal(data, &in)
		out = append(out, in.Text)
	}
	return out
}

// wakes returns the digests the supervisor's thread sv was given.
func (wt *wakeTest) wakes(t *testing.T, sv control.ThreadID) []string {
	t.Helper()
	var out []string
	for _, in := range wt.inputs(t, sv) {
		if strings.HasPrefix(in, "hataori: ") {
			out = append(out, in)
		}
	}
	return out
}

// turns counts the turns started in thread sv.
func (wt *wakeTest) turns(t *testing.T, sv control.ThreadID) int {
	t.Helper()
	var n int
	if err := wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'turn_started'`, int64(sv)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// woken waits until sv has n digests and its turn of the last ended, and
// returns them.
func (wt *wakeTest) woken(t *testing.T, owner *wire.Client, sv control.ThreadID, n int) []string {
	t.Helper()
	eventually(t, fmt.Sprintf("%d wakes", n), 20*time.Second, func() bool { return len(wt.wakes(t, sv)) >= n })
	eventually(t, "the supervisor's turn to end", 20*time.Second, func() bool {
		var ended int
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'turn_ended'`, int64(sv)).Scan(&ended)
		return ended == wt.turns(t, sv)
	})
	got := wt.wakes(t, sv)
	if len(got) != n {
		t.Fatalf("%d wakes, want %d: %q", len(got), n, got)
	}
	return got
}

// quiet moves the waker's clock past the quiet period, once its timer is
// set.
func (wt *wakeTest) quiet(t *testing.T) {
	t.Helper()
	eventually(t, "the quiet period to start", 10*time.Second, func() bool { return wt.clock().waiting() > 0 })
	wt.clock().advance(30 * time.Second)
}

// hour moves the waker's clock forward an hour, in steps of the quiet
// period, giving the waker a moment after each.
func (wt *wakeTest) hour() {
	for i := 0; i < 120; i++ {
		wt.clock().advance(30 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
}

// One turn end in the scope starts one supervisor turn, after the quiet
// period and not before; ten within the quiet period start one more, whose
// digest names each of them once.
func TestWakeOncePerTurnEndAndPerBurst(t *testing.T) {
	wt := newWakeTest(t)
	stop, owner, sv := wt.start(t)
	defer stop()
	a, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
	wt.read(t, pos)
	eventually(t, "the quiet period to start", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
	wt.clock().advance(30*time.Second - time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 0 {
		t.Fatalf("woken before the quiet period ended: %q", got)
	}
	wt.clock().advance(time.Millisecond)
	got := wt.woken(t, owner, sv.ID, 1)
	if !strings.HasPrefix(got[0], "hataori: a turn ended in 1 thread of this channel.\n") ||
		!strings.Contains(got[0], fmt.Sprintf("\nThread %d: reply alpha\nEnded: completed. Background tasks: 0.\nLast output:\n> alpha\n", a)) {
		t.Fatalf("the digest of one turn end:\n%s", got[0])
	}
	if n := wt.turns(t, sv.ID); n != 2 {
		t.Fatalf("%d supervisor turns after one wake, want 2 (its first and the wake)", n)
	}

	var ids []control.ThreadID
	var last uint64
	for i := 0; i < 10; i++ {
		id, err := owner.Start(context.Background(), control.StartRequest{Key: fmt.Sprintf("b%d", i), Scope: "c1",
			Thread: fmt.Sprintf("t-b%d", i), Repo: wt.repo, Input: control.Input{Text: fmt.Sprintf("reply beta %d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		last = max(last, wt.ended(t, id, 1))
	}
	wt.read(t, last)
	if n := wt.clock().waiting(); n != 1 {
		t.Fatalf("%d timers for ten turn ends, want one quiet period", n)
	}
	wt.quiet(t)
	got = wt.woken(t, owner, sv.ID, 2)
	digest := got[1]
	if !strings.HasPrefix(digest, "hataori: turns ended in 10 threads of this channel.\n") {
		t.Fatalf("the digest of ten:\n%s", digest)
	}
	for i, id := range ids {
		if n := strings.Count(digest, fmt.Sprintf("\nThread %d: ", id)); n != 1 {
			t.Fatalf("thread %d is named %d times in the digest:\n%s", id, n, digest)
		}
		if !strings.Contains(digest, fmt.Sprintf("> beta %d\n", i)) {
			t.Fatalf("thread %d's output is not in the digest:\n%s", id, digest)
		}
	}
	if strings.Contains(digest, fmt.Sprintf("Thread %d:", a)) {
		t.Fatalf("the digest names thread %d again, whose turn end woke the supervisor before:\n%s", a, digest)
	}
	if n := wt.turns(t, sv.ID); n != 3 {
		t.Fatalf("%d supervisor turns after two wakes, want 3", n)
	}
	wt.hour()
	time.Sleep(200 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 2 {
		t.Fatalf("%d wakes an hour later, want still 2", len(got))
	}
}

// Turn ends while the supervisor's turn runs are not sent into it: the
// send is refused as busy, and they start exactly one more turn after it
// ends.
func TestWakeAfterTheSupervisorsTurn(t *testing.T) {
	wt := newWakeTest(t)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	if err := owner.Send(ctx, sv.ID, "hold", control.Input{Text: "tool hold 6s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the supervisor's turn to run", 10*time.Second, func() bool {
		d, err := owner.Thread(ctx, sv.ID, 0, 0)
		return err == nil && d.State == control.Running
	})
	a, _ := wt.worker(t, owner, "c1", "a", "reply alpha")
	b, pos := wt.worker(t, owner, "c1", "b", "reply beta")
	wt.read(t, pos)
	wt.quiet(t)
	time.Sleep(300 * time.Millisecond)
	d, err := owner.Thread(ctx, sv.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != control.Running {
		t.Fatalf("the supervisor's turn ended before the quiet period did (%s): the machine is too slow for this test", d.State)
	}
	if got := wt.wakes(t, sv.ID); len(got) != 0 {
		t.Fatalf("a wake went into the supervisor's running turn: %q", got)
	}
	got := wt.woken(t, owner, sv.ID, 1)
	for _, id := range []control.ThreadID{a, b} {
		if n := strings.Count(got[0], fmt.Sprintf("\nThread %d: ", id)); n != 1 {
			t.Fatalf("thread %d is named %d times in the digest after the turn:\n%s", id, n, got[0])
		}
	}
	if n := wt.turns(t, sv.ID); n != 3 {
		t.Fatalf("%d supervisor turns, want 3: its first, the person's and one wake", n)
	}
	wt.hour()
	time.Sleep(200 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 1 {
		t.Fatalf("%d wakes an hour later, want still 1", len(got))
	}
}

// A turn end in the supervisor's own thread and one in another scope start
// nothing, and with every thread idle for an hour the supervisor gets no
// message and its agent no input.
func TestWakeNotByItsOwnThreadOtherScopesOrAnIdleHour(t *testing.T) {
	wt := newWakeTest(t)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	// The waker works here: one turn end in c1 wakes it once.
	_, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1)

	if err := owner.Send(ctx, sv.ID, "own", control.Input{Text: "reply hello"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	pos = wt.ended(t, sv.ID, 3)
	wt.worker(t, owner, "c2", "other", "reply elsewhere")
	wt.read(t, pos)
	time.Sleep(200 * time.Millisecond)
	if n := wt.clock().waiting(); n != 0 {
		t.Fatalf("%d timers after turn ends in the supervisor's own thread and in c2, want none", n)
	}
	inputs, turns := len(wt.inputs(t, sv.ID)), wt.turns(t, sv.ID)
	wt.hour()
	time.Sleep(300 * time.Millisecond)
	if n := len(wt.inputs(t, sv.ID)); n != inputs {
		t.Fatalf("the supervisor's thread got %d inputs in the idle hour, want none: %q", n-inputs, wt.inputs(t, sv.ID))
	}
	if n := wt.turns(t, sv.ID); n != turns {
		t.Fatalf("%d supervisor turns in the idle hour, want none", n-turns)
	}
	if got := wt.wakes(t, sv.ID); len(got) != 1 {
		t.Fatalf("%d wakes, want 1", len(got))
	}
}

// The daemon is stopped between a turn end and its wake, at each step of
// the wake: after the restart the supervisor is woken once for it, not
// twice and not never.
func TestWakeOnceAcrossARestart(t *testing.T) {
	for _, step := range []string{"quiet", "stored", "sent"} {
		t.Run(step, func(t *testing.T) {
			wt := newWakeTest(t)
			stopped := errors.New("stopped here")
			if step != "quiet" {
				h := func(at string) error {
					if at == step {
						return stopped
					}
					return nil
				}
				wt.hook.Store(&h)
			}
			stop, owner, sv := wt.start(t)
			a, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
			wt.read(t, pos)
			if step == "quiet" {
				eventually(t, "the quiet period to start", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
			} else {
				wt.quiet(t)
			}
			var sending sql.NullInt64
			eventually(t, "the waker to reach the step", 10*time.Second, func() bool {
				wt.db.QueryRow(`SELECT send_to FROM supervisor_wakes WHERE scope = 'c1'`).Scan(&sending)
				return step == "quiet" || sending.Valid
			})
			if step == "sent" {
				wt.woken(t, owner, sv.ID, 1)
			} else if got := wt.wakes(t, sv.ID); len(got) != 0 {
				t.Fatalf("woken before the restart: %q", got)
			}
			stop()

			stop, owner, _ = wt.start(t)
			defer stop()
			wt.read(t, pos)
			if step == "quiet" {
				wt.quiet(t)
			}
			got := wt.woken(t, owner, sv.ID, 1)
			if !strings.Contains(got[0], fmt.Sprintf("\nThread %d: reply alpha\n", a)) {
				t.Fatalf("the digest after the restart:\n%s", got[0])
			}
			eventually(t, "the wake to be stored as done", 10*time.Second, func() bool {
				var p int64
				wt.db.QueryRow(`SELECT pos, send_to FROM supervisor_wakes WHERE scope = 'c1'`).Scan(&p, &sending)
				return !sending.Valid && uint64(p) >= pos
			})
			wt.hour()
			time.Sleep(300 * time.Millisecond)
			if got := wt.wakes(t, sv.ID); len(got) != 1 {
				t.Fatalf("%d wakes after the restart and an hour, want 1: %q", len(got), got)
			}
			if n := wt.turns(t, sv.ID); n != 2 {
				t.Fatalf("%d supervisor turns, want 2: its first and one wake", n)
			}
		})
	}
}

// A turn end while the supervisor's thread waits on a question is not sent
// to it, since the send would be the answer: it wakes the supervisor once,
// after a person answered and that turn ended.
func TestWakeNotIntoAnOpenQuestion(t *testing.T) {
	wt := newWakeTest(t)
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	if err := owner.Send(ctx, sv.ID, "ask", control.Input{Text: "ask Go on? -- yes | no"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the supervisor's question", 10*time.Second, func() bool {
		d, err := owner.Thread(ctx, sv.ID, 0, 0)
		return err == nil && d.State == control.Waiting
	})
	a, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
	wt.read(t, pos)
	wt.quiet(t)
	time.Sleep(300 * time.Millisecond)
	d, err := owner.Thread(ctx, sv.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != control.Waiting || len(d.Requests) != 1 {
		t.Fatalf("the supervisor's thread is %s with %d requests after the quiet period; want its question still open", d.State, len(d.Requests))
	}
	if got := wt.wakes(t, sv.ID); len(got) != 0 {
		t.Fatalf("woken while its question was open: %q", got)
	}
	if err := owner.Send(ctx, sv.ID, "answer", control.Input{Text: "yes"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	got := wt.woken(t, owner, sv.ID, 1)
	if !strings.Contains(got[0], fmt.Sprintf("\nThread %d: reply alpha\n", a)) {
		t.Fatalf("the digest after the answer:\n%s", got[0])
	}
	var answers []string
	rows, err := wt.db.Query(`SELECT data FROM events WHERE thread_id = ? AND kind = 'answer' ORDER BY pos`, int64(sv.ID))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var data []byte
		rows.Scan(&data)
		var in struct{ Text string }
		json.Unmarshal(data, &in)
		answers = append(answers, in.Text)
	}
	rows.Close()
	if len(answers) != 1 || answers[0] != "yes" {
		t.Fatalf("the supervisor's question was answered with %q, want only the person's \"yes\"", answers)
	}
}

// The turn end of an earlier supervisor's thread, one a person closed and
// the daemon replaced, does not wake the new supervisor, though it is read
// again after the restart while the new thread is the current one.
func TestWakeNotByAnEarlierSupervisor(t *testing.T) {
	wt := newWakeTest(t)
	stop, owner, first := wt.start(t)
	ctx := context.Background()
	a, _ := wt.worker(t, owner, "c1", "a", "reply alpha")
	if err := owner.Send(ctx, first.ID, "own", control.Input{Text: "reply hello"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	pos := wt.ended(t, first.ID, 2)
	wt.read(t, pos)
	if n := wt.clock().waiting(); n != 1 {
		t.Fatalf("%d timers, want the quiet period of thread %d's turn end", n, a)
	}
	if err := owner.CloseThread(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	stop()
	// The new daemon's waker reads both turn ends again, once the
	// supervisor's new thread is there.
	db := wt.db
	b := func() { // on the daemon's goroutine: no t.Fatal
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var n int
			db.QueryRow(`SELECT count(*) FROM threads WHERE scope = 'c1' AND confined = 1`).Scan(&n)
			if n == 2 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("timed out waiting for the supervisor's new thread")
				return
			}
		}
	}
	wt.before.Store(&b)
	stop, owner, sv := wt.start(t)
	defer stop()
	if sv.ID == first.ID {
		t.Fatalf("the closed supervisor %d was not replaced", first.ID)
	}
	wt.read(t, pos)
	wt.quiet(t)
	got := wt.woken(t, owner, sv.ID, 1)
	if !strings.Contains(got[0], fmt.Sprintf("\nThread %d: reply alpha\n", a)) || strings.Contains(got[0], fmt.Sprintf("Thread %d", first.ID)) {
		t.Fatalf("the new supervisor's digest; want thread %d and not the earlier supervisor %d:\n%s", a, first.ID, got[0])
	}
}
