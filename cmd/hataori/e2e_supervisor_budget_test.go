package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// budget sets c1's supervisor budget: each limit, 0 for off.
func budget(turns, sends int, tokens int64) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		sv := cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)
		sv["turns_per_hour"], sv["sends_per_hour"], sv["tokens_per_day"] = turns, sends, tokens
	}
}

// notices returns the budget notices posted in the supervisor's thread.
func (wt *wakeTest) notices(sv control.ThreadInfo) []string {
	var out []string
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	for _, c := range wt.r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "budget for this channel is used up") {
			out = append(out, c.Message.Text)
		}
	}
	return out
}

// noticed waits for n notices in the supervisor's thread, gives the
// outbox a moment for any more, and returns them.
func (wt *wakeTest) noticed(t *testing.T, sv control.ThreadInfo, n int) []string {
	t.Helper()
	eventually(t, fmt.Sprintf("%d budget notices", n), 10*time.Second, func() bool { return len(wt.notices(sv)) >= n })
	time.Sleep(300 * time.Millisecond)
	got := wt.notices(sv)
	if len(got) != n {
		t.Fatalf("%d budget notices, want %d: %q", len(got), n, got)
	}
	return got
}

// until moves the clock to at, firing what is due. The clock does not go
// back: at must not be before it.
func (wt *wakeTest) until(t *testing.T, at time.Time) {
	t.Helper()
	d := at.Sub(wt.clock().Now())
	if d < 0 {
		t.Fatalf("moving the clock back by %v, to %v", -d, at)
	}
	wt.clock().advance(d)
}

// held waits until a turn end read up to pos has been held by the budget:
// its quiet period passed, and only the hold's timer is left.
func (wt *wakeTest) held(t *testing.T, pos uint64) {
	t.Helper()
	wt.read(t, pos)
	wt.quiet(t)
	eventually(t, "the budget's hold", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
}

// At the turn limit no supervisor turn starts, and a person is told once,
// also after a restart, which keeps the count; the turn ends after it
// start one turn when the next hour starts.
func TestBudgetTurns(t *testing.T) {
	wt := newWakeTest(t, budget(3, 0, 0))
	stop, owner, sv := wt.start(t)
	// Its first turn, and two wakes: three turns this hour.
	for _, k := range []string{"a", "b"} {
		_, pos := wt.worker(t, owner, "c1", k, "reply "+k)
		wt.read(t, pos)
		wt.quiet(t)
	}
	wt.woken(t, owner, sv.ID, 2)
	c, pos := wt.worker(t, owner, "c1", "c", "reply c")
	wt.held(t, pos)
	if got := wt.wakes(t, sv.ID); len(got) != 2 {
		t.Fatalf("%d wakes at the turn limit, want still 2", len(got))
	}
	got := wt.noticed(t, sv, 1)
	if !strings.Contains(got[0], "3 of 3 turns this hour") || !strings.Contains(got[0], "until "+wakeBase.Add(time.Hour).Format("2006-01-02 15:04 MST")) ||
		strings.Contains(got[0], "refused") {
		t.Fatalf("the notice: %s", got[0])
	}
	d, _ := wt.worker(t, owner, "c1", "d", "reply d")
	stop()

	stop, owner, _ = wt.start(t)
	defer stop()
	_, pos = wt.worker(t, owner, "c1", "e", "reply e")
	wt.held(t, pos)
	if got := wt.wakes(t, sv.ID); len(got) != 2 {
		t.Fatalf("%d wakes after the restart, in the same hour; want still 2", len(got))
	}
	if n := wt.turns(t, sv.ID); n != 3 {
		t.Fatalf("%d supervisor turns this hour, want 3", n)
	}
	wt.noticed(t, sv, 1)

	wt.until(t, wakeBase.Add(time.Hour))
	got = wt.woken(t, owner, sv.ID, 3)
	for _, id := range []control.ThreadID{c, d} {
		if !strings.Contains(got[2], fmt.Sprintf("\nThread %d: ", id)) {
			t.Fatalf("thread %d is not in the wake of the next hour:\n%s", id, got[2])
		}
	}
	wt.hour()
	time.Sleep(200 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 3 {
		t.Fatalf("%d wakes, want 3", len(got))
	}
}

// At the token limit, counted from the agent's usage as Tokens.Today
// counts it, no supervisor turn starts until the next day.
func TestBudgetTokens(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 1000))
	nextDay := time.Date(wakeBase.Year(), wakeBase.Month(), wakeBase.Day()+1, 0, 0, 0, 0, time.Local)
	stop, owner, sv := wt.start(t)
	defer stop()
	// One turn reports twice: totals of 600, then 800, which adds 800.
	sendAndRead(t, owner, sv.ID, "u1", "usage 400 100 50 50 0 0\nusage 600 100 50 50 0 0\nreply spent")
	sendAndRead(t, owner, sv.ID, "u2", "usage 500 0 0 0 0 0\nreply spent")
	th, err := owner.Thread(context.Background(), sv.ID, 0, 0)
	if err != nil || th.Tokens == nil || th.Tokens.Today != 1300 {
		t.Fatalf("the supervisor's tokens today %+v, %v; want 1300", th.Tokens, err)
	}
	_, pos := wt.worker(t, owner, "c1", "a", "reply a")
	wt.held(t, pos)
	got := wt.noticed(t, sv, 1)
	if !strings.Contains(got[0], "1300 of 1000 tokens today") ||
		!strings.Contains(got[0], "until "+nextDay.Format("2006-01-02 15:04 MST")) {
		t.Fatalf("the notice: %s", got[0])
	}
	// The clock starts on the next hour of the real time, so run between
	// 22:00 and 23:00 it starts in the last hour before the next day and
	// already passed nextDay - 1h.
	if at := nextDay.Add(-time.Hour); at.After(wt.clock().Now()) {
		wt.until(t, at)
	}
	time.Sleep(300 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 0 {
		t.Fatalf("woken before the next day: %q", got)
	}
	// The host sleeps through the rest of the day: no timer counts that,
	// but the hold ends within a minute by the wall clock.
	wt.clock().sleep(nextDay.Sub(wt.clock().Now()))
	time.Sleep(300 * time.Millisecond)
	if got := wt.wakes(t, sv.ID); len(got) != 0 {
		t.Fatalf("woken before a timer fired: %q", got)
	}
	wt.clock().advance(time.Minute)
	wt.woken(t, owner, sv.ID, 1)
	wt.noticed(t, sv, 1)
}

// At the sends limit, the supervisor's sends and starts are refused at the
// call, a start counting once; a person is told once; the count holds
// across a restart; and they work again in the next hour.
func TestBudgetSends(t *testing.T) {
	wt := newWakeTest(t, budget(0, 2, 0))
	stop, owner, sv := wt.start(t)
	w, _ := wt.worker(t, owner, "c1", "w", "reply w")
	call := func(key, script string) string { return sendAndRead(t, owner, sv.ID, key, script) }
	send := func(text string) string {
		return fmt.Sprintf("mcp send_to_thread {\"number\": %d, \"text\": %q}", w, text)
	}

	if a := call("s1", fmt.Sprintf("mcp start_thread {\"repo\": %q, \"text\": \"reply started\"}", wt.repo)); strings.HasPrefix(a, "mcp error") {
		t.Fatalf("the first start: %s", a)
	}
	if a := call("s2", send("reply one")); strings.HasPrefix(a, "mcp error") {
		t.Fatalf("the send after one start: %s", a)
	}
	wt.ended(t, w, 2)
	a := call("s3", send("reply two"))
	if !strings.HasPrefix(a, "mcp error: not allowed: over budget: 2 of 2 sends and starts this hour") {
		t.Fatalf("the third send: %s", a)
	}
	if a := call("s4", fmt.Sprintf("mcp start_thread {\"repo\": %q, \"text\": \"reply again\"}", wt.repo)); !strings.HasPrefix(a, "mcp error: not allowed: over budget") {
		t.Fatalf("a start over budget: %s", a)
	}
	got := wt.noticed(t, sv, 1)
	if !strings.Contains(got[0], "2 of 2 sends and starts this hour") ||
		!strings.Contains(got[0], "Its sends and starts are refused until "+wakeBase.Add(time.Hour).Format("2006-01-02 15:04 MST")) {
		t.Fatalf("the notice: %s", got[0])
	}
	stop()

	stop, owner, _ = wt.start(t)
	defer stop()
	if a := call("s5", send("reply three")); !strings.HasPrefix(a, "mcp error: not allowed: over budget: 2 of 2") {
		t.Fatalf("a send after the restart, in the same hour: %s", a)
	}
	wt.noticed(t, sv, 1)
	if n := len(threadEvents(t, owner, w, "turn_ended")); n != 2 {
		t.Fatalf("the worker ran %d turns, want 2: its first and the one send", n)
	}
	// The next hour: the held turn ends wake it first, then it may send.
	wt.until(t, wakeBase.Add(time.Hour))
	wt.woken(t, owner, sv.ID, 1)
	if a := call("s6", send("reply four")); strings.HasPrefix(a, "mcp error") {
		t.Fatalf("a send in the next hour: %s", a)
	}
	wt.ended(t, w, 3)
}
