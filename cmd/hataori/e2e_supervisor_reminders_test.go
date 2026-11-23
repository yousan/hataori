package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #649: a reminder, a wake at a time the
// supervisor sets. End to end with the fake agent and the wake test's
// clock.

// remindersMax sets c1's reminders_max.
func remindersMax(n int) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["reminders_max"] = n
	}
}

// remindTest is a wake test, and what makes a connection with the token of
// c1's supervisor.
func remindTest(t *testing.T, more ...func(cfg map[string]any)) (*wakeTest, func() *wire.Client) {
	t.Helper()
	var (
		mu    sync.Mutex
		issue func(string) (string, error)
	)
	supervisorHook = func(i func(string) (string, error), _ func(string)) { mu.Lock(); issue = i; mu.Unlock() }
	t.Cleanup(func() { supervisorHook = nil })
	wt := newWakeTest(t, more...)
	return wt, func() *wire.Client {
		mu.Lock()
		defer mu.Unlock()
		return svConn(t, wt.r.root, issue)
	}
}

// remindIn sets a reminder on the supervisor's connection c.
func remindIn(t *testing.T, c *wire.Client, d time.Duration, note string) control.RemindAnswer {
	t.Helper()
	a, err := c.Remind(context.Background(), control.RemindRequest{Delay: int64(d / time.Second), Note: note})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The supervisor sets a reminder with its tool. It wakes it once, at its
// time and not before, with its note, under a key of its own, and lists it
// as open in a wake before.
func TestReminderWakesOnceAtItsTime(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	defer stop()
	set := wt.clock().Now()
	if a := sendAndRead(t, owner, sv.ID, "p", `mcp set_reminder {"minutes": 120, "note": "reply looked again"}`); !strings.HasPrefix(a, "mcp: Reminder 1 is set for ") {
		t.Fatalf("set_reminder: %s", a)
	}
	_, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
	wt.read(t, pos)
	got := wt.wokenN(t, owner, sv.ID, 1)
	due := set.Add(2 * time.Hour).Format("2006-01-02 15:04 MST")
	if want := "\nOpen reminders:\n- 1, due " + due + ": reply looked again\n"; !strings.HasSuffix(got[0], want) {
		t.Fatalf("the wake before it; want its end to be %q:\n%s", want, got[0])
	}
	wt.noWake(t, sv.ID, set.Add(2*time.Hour-30*time.Second).Sub(wt.clock().Now()), 1, "before its time")
	wt.steps(30 * time.Second)
	got = wt.woken(t, owner, sv.ID, 2)
	want := "hataori: 1 reminder of yours came due.\n\nReminder 1, due " + due + ", on time:\n> reply looked again\n"
	if got[1] != want {
		t.Fatalf("the reminder's wake:\n%s\nwant:\n%s", got[1], want)
	}
	var key string
	wt.db.QueryRow(`SELECT json_extract(data, '$.key') FROM events WHERE thread_id = ? AND kind = 'turn_started' ORDER BY pos DESC LIMIT 1`,
		int64(sv.ID)).Scan(&key)
	if ids := supervisor.WakeReminders(key); !strings.HasPrefix(key, supervisor.WakeKeyPrefix) || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("the key of the reminder's wake: %q", key)
	}
	wt.noWake(t, sv.ID, 3*time.Hour, 2, "after it")
	// The next wake does not give it again.
	_, pos = wt.worker(t, owner, "c1", "b", "reply beta")
	wt.read(t, pos)
	if got := wt.wokenN(t, owner, sv.ID, 3); strings.Contains(got[2], "eminder") {
		t.Fatalf("the wake after the reminder's:\n%s", got[2])
	}
	wt.noWake(t, sv.ID, time.Hour, 3, "nor does one after that")
}

// A reminder that came due while the daemon was down wakes the supervisor
// once after the start, saying how late. A daemon killed between storing
// that wake and sending it, or storing its position, gives it once.
func TestReminderOnceAcrossARestart(t *testing.T) {
	for _, step := range []string{"down", "stored", "sent"} {
		t.Run(step, func(t *testing.T) {
			wt, conn := remindTest(t, budget(0, 0, 0))
			stop, owner, sv := wt.start(t)
			remindIn(t, conn(), time.Hour, "reply looked again")
			stop()
			wt.clock().advance(3*time.Hour + 10*time.Minute)
			if step != "down" {
				stopped := errors.New("stopped here")
				h := func(at string) error {
					if at == step {
						return stopped
					}
					return nil
				}
				wt.hook.Store(&h)
				stop, _, _ = wt.start(t)
				var sending sql.NullInt64
				eventually(t, "the waker to reach the step", 10*time.Second, func() bool {
					wt.db.QueryRow(`SELECT send_to FROM supervisor_wakes WHERE scope = 'c1'`).Scan(&sending)
					return sending.Valid
				})
				if step == "sent" {
					wt.woken(t, owner, sv.ID, 1)
				} else if got := wt.wakes(t, sv.ID); len(got) != 0 {
					t.Fatalf("woken before the restart: %q", got)
				}
				// A stored wake gives it: it is not open any more.
				if _, err := conn().Remind(context.Background(), control.RemindRequest{Cancel: 1}); err == nil || !strings.Contains(err.Error(), "is being given") {
					t.Fatalf("cancelling a reminder that a stored wake carries: %v", err)
				}
				stop()
			}
			stop, owner, _ = wt.start(t)
			defer stop()
			got := wt.wokenN(t, owner, sv.ID, 1)
			if !strings.HasPrefix(got[0], "hataori: 1 reminder of yours came due.\n\nReminder 1, due ") ||
				!strings.Contains(got[0], ", 2 hours 10 minutes late:\n> reply looked again\n") {
				t.Fatalf("the wake after the restart:\n%s", got[0])
			}
			wt.noWake(t, sv.ID, 2*time.Hour, 1, "after the restart")
		})
	}
}

// A reminder that comes due while turn ends are folded goes into their
// wake, and one set by a thread that a person closed is given to the thread
// that replaces it.
func TestReminderIsFoldedAndOutlivesTheThread(t *testing.T) {
	wt, conn := remindTest(t, budget(0, 0, 0))
	stop, owner, first := wt.start(t)
	remindIn(t, conn(), 10*time.Minute, "reply folded")
	remindIn(t, conn(), 5*time.Hour, "reply for the next")
	timers := func(n int) {
		t.Helper()
		eventually(t, fmt.Sprintf("%d timers", n), 10*time.Second, func() bool { return wt.clock().waiting() == n })
	}
	eventually(t, "the reminders' timer", 10*time.Second, func() bool { return wt.clock().waiting() > 0 })
	wt.steps(9*time.Minute + 45*time.Second)
	timers(1) // the reminder's
	a, pos := wt.worker(t, owner, "c1", "a", "reply alpha")
	wt.read(t, pos)
	timers(2)                            // and the quiet period's
	wt.clock().advance(20 * time.Second) // the reminder is due, the quiet period runs
	time.Sleep(300 * time.Millisecond)
	if n := wt.taken(t, first.ID); n != 0 {
		t.Fatalf("%d wakes while a turn end is folded, want none yet", n)
	}
	wt.clock().advance(10 * time.Second)
	got := wt.woken(t, owner, first.ID, 1)
	if !strings.HasPrefix(got[0], "hataori: a turn ended in 1 thread of this channel, and 1 reminder of yours came due.\n") ||
		!strings.Contains(got[0], fmt.Sprintf("\nThread %d: reply alpha\n", a)) || !strings.Contains(got[0], "\nReminder 1, due ") ||
		!strings.Contains(got[0], "> reply folded\n") || !strings.Contains(got[0], "\nOpen reminders:\n- 2, due ") {
		t.Fatalf("the wake of both:\n%s", got[0])
	}
	if err := owner.CloseThread(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	stop()
	stop, owner, sv := wt.start(t)
	defer stop()
	if sv.ID == first.ID {
		t.Fatalf("the closed supervisor %d was not replaced", first.ID)
	}
	if got := wt.wokenN(t, owner, sv.ID, 1); !strings.Contains(got[0], "\nReminder 2, due ") || !strings.Contains(got[0], "> reply for the next\n") {
		t.Fatalf("the replacing thread's wake:\n%s", got[0])
	}
}

// With the budget used up a due reminder waits, and wakes the supervisor
// once the budget allows it. A reminder that is set is one send of it.
func TestReminderWaitsForTheBudget(t *testing.T) {
	wt, conn := remindTest(t, budget(1, 3, 0))
	stop, owner, sv := wt.start(t) // its first turn is the hour's
	defer func() { stop() }()
	c := conn()
	remindIn(t, c, 10*time.Minute, "reply looked again")
	second := remindIn(t, c, 6*time.Hour, "later")
	if _, err := c.Remind(context.Background(), control.RemindRequest{Cancel: second.Number}); err != nil {
		t.Fatal(err) // a cancel is no send
	}
	remindIn(t, c, 6*time.Hour, "later")
	over := func(c *wire.Client, when string) {
		t.Helper()
		_, err := c.Remind(context.Background(), control.RemindRequest{Delay: 600, Note: "one more"})
		if !errors.Is(err, control.ErrForbidden) || !strings.Contains(err.Error(), "3 of 3 sends") {
			t.Fatalf("a reminder past sends_per_hour, %s: %v", when, err)
		}
	}
	over(c, "of the same daemon")
	stop()
	stop, owner, sv = wt.start(t)
	over(conn(), "after a restart")
	wt.noWake(t, sv.ID, 59*time.Minute, 0, "with the budget used up")
	wt.until(t, wakeBase.Add(time.Hour))
	if got := wt.wokenN(t, owner, sv.ID, 1); !strings.Contains(got[0], "\nReminder 1, due ") || !strings.Contains(got[0], "minutes late:\n> reply looked again\n") {
		t.Fatalf("the wake of the next hour:\n%s", got[0])
	}
	wt.noWake(t, sv.ID, 2*time.Hour, 1, "after it")
}

// A time less than 5 minutes or more than 7 days ahead, a note over 1 KB,
// and one reminder more than reminders_max are refused; one cancelled never
// wakes, and is cancelled once. The core answers the owner not_supported.
func TestReminderRefusedAndCancelled(t *testing.T) {
	wt, conn := remindTest(t, budget(0, 0, 0), remindersMax(2))
	stop, owner, sv := wt.start(t)
	defer stop()
	c, ctx := conn(), context.Background()
	at := func(d time.Duration) *time.Time { x := wt.clock().Now().Add(d); return &x }
	const week = 7 * 24 * time.Hour
	for name, r := range map[string]struct {
		r    control.RemindRequest
		want string
	}{
		"a delay under 5 minutes": {control.RemindRequest{Delay: 299, Note: "x"}, "from 5 minutes to 7 days"},
		"a delay over 7 days":     {control.RemindRequest{Delay: int64(week/time.Second) + 1, Note: "x"}, "from 5 minutes to 7 days"},
		"a delay out of range":    {control.RemindRequest{Delay: 1 << 62, Note: "x"}, "from 5 minutes to 7 days"},
		"a negative delay":        {control.RemindRequest{Delay: -600, Note: "x"}, "from 5 minutes to 7 days"},
		"a time under 5 minutes":  {control.RemindRequest{At: at(5*time.Minute - time.Second), Note: "x"}, "from 5 minutes to 7 days"},
		"a time over 7 days":      {control.RemindRequest{At: at(week + time.Second), Note: "x"}, "from 5 minutes to 7 days"},
		"a note over 1 KB":        {control.RemindRequest{Delay: 600, Note: strings.Repeat("x", 1025)}, "at most 1024"},
		"no note":                 {control.RemindRequest{Delay: 600}, "needs a note"},
		"a time and a delay":      {control.RemindRequest{At: at(time.Hour), Delay: 600, Note: "x"}, "only one of them"},
		"nothing":                 {control.RemindRequest{Note: "x"}, "only one of them"},
		"a cancel of none":        {control.RemindRequest{Cancel: 9}, "no open reminder 9"},
	} {
		if _, err := c.Remind(ctx, r.r); err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("%s: %v, want %q", name, err, r.want)
		}
	}
	// The bounds themselves are taken.
	if _, err := c.Remind(ctx, control.RemindRequest{At: at(5 * time.Minute), Note: strings.Repeat("x", 1024)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remind(ctx, control.RemindRequest{Delay: int64(week / time.Second), Note: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remind(ctx, control.RemindRequest{Delay: 600, Note: "x"}); err == nil || !strings.Contains(err.Error(), "reminders_max is 2") {
		t.Fatalf("a third open reminder: %v", err)
	}
	for _, n := range []int64{1, 2} {
		if a, err := c.Remind(ctx, control.RemindRequest{Cancel: n}); err != nil || a.Number != n {
			t.Fatalf("cancelling reminder %d: %+v, %v", n, a, err)
		}
		if _, err := c.Remind(ctx, control.RemindRequest{Cancel: n}); err == nil {
			t.Fatalf("reminder %d was cancelled twice", n)
		}
	}
	if a := remindIn(t, c, 10*time.Minute, "x"); a.Number != 3 {
		t.Fatalf("the next reminder is %d, want 3", a.Number)
	}
	if a := sendAndRead(t, owner, sv.ID, "p", `mcp cancel_reminder {"number": 3}`); a != "mcp: Reminder 3 is cancelled." {
		t.Fatalf("cancel_reminder: %s", a)
	}
	wt.noWake(t, sv.ID, 2*time.Hour, 0, "with every reminder cancelled")
	if _, err := owner.Remind(ctx, control.RemindRequest{Delay: 600, Note: "x"}); !errors.Is(err, control.ErrNotSupported) {
		t.Fatalf("the owner's Remind: %v, want not supported", err)
	}
}

// With no reminder set and no event the supervisor gets no input for an
// hour, and the waker waits on no timer.
func TestNoReminderNoInput(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, _, sv := wt.start(t)
	defer stop()
	inputs := len(wt.inputs(t, sv.ID))
	wt.hour()
	time.Sleep(200 * time.Millisecond)
	if n := len(wt.inputs(t, sv.ID)); n != inputs {
		t.Fatalf("%d inputs in the idle hour: %q", n-inputs, wt.inputs(t, sv.ID))
	}
	if n := wt.clock().waiting(); n != 0 {
		t.Fatalf("%d timers with no reminder and no event, want none", n)
	}
}

// stopAt runs the daemon until the waker stops at step of a wake, and stops
// it.
func (wt *wakeTest) stopAt(t *testing.T, step string) {
	t.Helper()
	stopped := errors.New("stopped here")
	h := func(at string) error {
		if at == step {
			return stopped
		}
		return nil
	}
	wt.hook.Store(&h)
	stop, _, _ := wt.start(t)
	eventually(t, "the wake to be "+step, 10*time.Second, func() bool {
		var sending sql.NullInt64
		wt.db.QueryRow(`SELECT send_to FROM supervisor_wakes WHERE scope = 'c1'`).Scan(&sending)
		return sending.Valid
	})
	stop()
}

// A reminder whose stored wake is refused as busy, since a person's turn
// started in between, is given after that turn.
func TestReminderAfterABusyRefusal(t *testing.T) {
	wt, conn := remindTest(t, budget(0, 0, 0))
	stop, _, sv := wt.start(t)
	remindIn(t, conn(), time.Hour, "reply looked again")
	stop()
	wt.clock().advance(3 * time.Hour)
	wt.stopAt(t, "stored")
	// Right before the stored wake is sent, a person's message comes.
	var once sync.Once
	prev := wakerHook
	wakerHook = func(w *supervisor.Waker) {
		prev(w)
		before := w.Before
		w.Before = func(ctx context.Context, id control.ThreadID) {
			once.Do(func() {
				owner, err := wire.Dial(wt.r.root)
				if err != nil {
					t.Error(err)
					return
				}
				defer owner.Close()
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					if d, err := owner.Thread(ctx, id, 0, 0); err == nil && d.State == control.Running {
						return
					}
					owner.Send(ctx, id, "hold", control.Input{Text: "tool hold 2s"}, control.Refuse)
				}
				t.Error("the person's turn did not start")
			})
			before(ctx, id)
		}
	}
	stop, owner, _ := wt.start(t)
	defer stop()
	got := wt.woken(t, owner, sv.ID, 1)
	if !strings.Contains(got[0], "\nReminder 1, due ") || !strings.Contains(got[0], "> reply looked again\n") {
		t.Fatalf("the wake after the person's turn:\n%s", got[0])
	}
	wt.noWake(t, sv.ID, 2*time.Hour, 1, "after it")
}

// A due reminder does not go past the budget's hold with a thread that a
// person asked for (#677); that wake lists it as open, and it wakes when
// the hold ends.
func TestReminderStaysBehindAnAskedThread(t *testing.T) {
	wt, conn := remindTest(t, budget(1, 0, 0), autonomy("act"))
	stop, owner, sv := wt.start(t) // its first turn is the hour's
	defer stop()
	remindIn(t, conn(), 10*time.Minute, "reply looked again")
	wt.noWake(t, sv.ID, 11*time.Minute, 0, "with the budget used up")
	w, pos := wt.asks(t, owner, sv, "q1", "reply the answer")
	wt.read(t, pos)
	time.Sleep(200 * time.Millisecond)
	wt.steps(time.Minute)
	got := wt.woken(t, owner, sv.ID, 1)
	if !names(got[0], w) || strings.Contains(got[0], "\nReminder 1,") || !strings.Contains(got[0], "\nOpen reminders:\n- 1, due ") {
		t.Fatalf("the wake past the hold:\n%s", got[0])
	}
	wt.noWake(t, sv.ID, 20*time.Minute, 1, "until the hold ends")
	wt.until(t, wakeBase.Add(time.Hour))
	if got := wt.wokenN(t, owner, sv.ID, 2); !strings.Contains(got[1], "\nReminder 1, due ") {
		t.Fatalf("the wake of the next hour:\n%s", got[1])
	}
}

// A reminder's wake stored for a thread that a person then closed goes to
// the thread that replaces it, once: no wake after gives it again.
func TestReminderOfAStoredWakeGoesToANewThread(t *testing.T) {
	wt, conn := remindTest(t, budget(0, 0, 0))
	stop, _, first := wt.start(t)
	remindIn(t, conn(), time.Hour, "reply looked again")
	stop()
	wt.clock().advance(2 * time.Hour)
	wt.stopAt(t, "stored")
	// Closed while the waker is stopped.
	hold := make(chan struct{})
	prev := wakerHook
	wakerHook = func(w *supervisor.Waker) {
		prev(w)
		w.Before = func(context.Context, control.ThreadID) { <-hold }
	}
	s := runDaemon(t, wt.r.root)
	owner, err := wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the supervisor's thread to close", 10*time.Second, func() bool { return owner.CloseThread(context.Background(), first.ID) == nil })
	owner.Close()
	close(hold)
	s()
	wakerHook = prev
	if n := wt.taken(t, first.ID); n != 0 {
		t.Fatalf("the closed thread took %d wakes, want none", n)
	}
	stop, owner, sv := wt.start(t)
	defer stop()
	if sv.ID == first.ID {
		t.Fatalf("the closed supervisor %d was not replaced", first.ID)
	}
	if got := wt.wokenN(t, owner, sv.ID, 1); !strings.Contains(got[0], "\nReminder 1, due ") {
		t.Fatalf("the replacing thread's wake:\n%s", got[0])
	}
	var key string
	wt.db.QueryRow(`SELECT json_extract(data, '$.key') FROM events WHERE thread_id = ? AND kind = 'turn_started' ORDER BY pos DESC LIMIT 1`,
		int64(sv.ID)).Scan(&key)
	if _, to, _ := supervisor.ParseWakeKey(key); to != sv.ID || len(supervisor.WakeReminders(key)) != 1 {
		t.Fatalf("the key of the wake given to thread %d: %q", sv.ID, key)
	}
	_, pos := wt.worker(t, owner, "c1", "b", "reply beta")
	wt.read(t, pos)
	if got := wt.wokenN(t, owner, sv.ID, 2); strings.Contains(got[1], "eminder") {
		t.Fatalf("the wake after it:\n%s", got[1])
	}
	wt.noWake(t, sv.ID, time.Hour, 2, "after that one")
}

// A reminder cancelled after its wake was made, and before that was stored,
// is not given: the wake is made again without it.
func TestReminderCancelledWhileItsWakeIsMade(t *testing.T) {
	wt, conn := remindTest(t, budget(0, 0, 0))
	var c *wire.Client
	prev := wakerHook
	var once sync.Once
	wakerHook = func(w *supervisor.Waker) {
		prev(w)
		sleep := w.Sleep // between making a wake and storing it
		w.Sleep = func(ctx context.Context, id control.ThreadID) (bool, error) {
			once.Do(func() {
				if _, err := c.Remind(ctx, control.RemindRequest{Cancel: 1}); err != nil {
					t.Error(err)
				}
			})
			return sleep(ctx, id)
		}
	}
	stop, log := runDaemonLog(t, wt.r.root)
	defer stop()
	owner, err := wire.Dial(wt.r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	wt.db = wt.r.db(t)
	sv := supervisorReady(t, owner, "c1")
	c = conn()
	remindIn(t, c, 10*time.Minute, "reply cancelled")
	remindIn(t, c, 10*time.Minute, "reply kept")
	got := wt.wokenN(t, owner, sv.ID, 1)
	if strings.Contains(got[0], "cancelled") || !strings.HasPrefix(got[0], "hataori: 1 reminder of yours came due.\n\nReminder 2, due ") {
		t.Fatalf("the wake:\n%s", got[0])
	}
	if l := log(); strings.Contains(l, "a reminder of the wake was cancelled") {
		t.Fatalf("the waker stopped on it:\n%s", l)
	}
}

// stops stands in for a plugin that stops each input that has text, and
// calls first when it does.
type stops struct {
	text string
	did  func()
}

func (s stops) Run(_ context.Context, c control.HookCall) control.HookResult {
	if strings.Contains(c.Text, s.text) {
		s.did()
		return control.HookResult{Text: c.Text, Stopped: true, By: "stopper", Reason: "stopped"}
	}
	return control.HookResult{Text: c.Text}
}

// A wake that a plugin stops takes its reminder with it, and leaves one
// that came due meanwhile, which then wakes.
func TestReminderAfterAWakeAPluginStopped(t *testing.T) {
	wt, conn := remindTest(t, budget(0, 0, 0))
	prev := coreHook
	var once sync.Once
	coreHook = func(c *core.Core) {
		prev(c)
		c.Hooks = stops{"stop this", func() { once.Do(func() { wt.clock().advance(time.Hour) }) }}
	}
	stop, owner, sv := wt.start(t)
	defer stop()
	c := conn()
	remindIn(t, c, 10*time.Minute, "stop this")
	remindIn(t, c, 30*time.Minute, "reply looked again") // due while the first is being stopped
	got := wt.wokenN(t, owner, sv.ID, 1)
	if !strings.Contains(got[0], "\nReminder 2, due ") || strings.Contains(got[0], "Reminder 1") {
		t.Fatalf("the wake:\n%s", got[0])
	}
	wt.noWake(t, sv.ID, 2*time.Hour, 1, "after it")
}
