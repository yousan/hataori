package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #672: a worker's turn end that completed with
// background tasks still running wakes nobody; the thread's next turn end
// does, or an hour without one. End to end with the fake agent, whose
// "background D" ends the turn with one background task and starts its
// next turn by itself D later (by the real clock), and the wake test's
// clock.

const staleLine = "Its background work has not come back"

// steps moves the waker's clock on by d, half a minute at a time.
func (wt *wakeTest) steps(d time.Duration) {
	for ; d > 0; d -= 30 * time.Second {
		wt.clock().advance(min(d, 30*time.Second))
		time.Sleep(3 * time.Millisecond)
	}
}

// wokenN moves the clock on until sv has n wakes, and returns them.
func (wt *wakeTest) wokenN(t *testing.T, owner *wire.Client, sv control.ThreadID, n int) []string {
	t.Helper()
	wt.settle(t, fmt.Sprintf("%d wakes", n), func() bool { return len(wt.wakes(t, sv)) >= n })
	return wt.woken(t, owner, sv, n)
}

// noWake moves the clock on by d and checks that sv was woken want times.
func (wt *wakeTest) noWake(t *testing.T, sv control.ThreadID, d time.Duration, want int, what string) {
	t.Helper()
	wt.steps(d)
	time.Sleep(200 * time.Millisecond)
	if got := wt.wakes(t, sv); len(got) != want {
		t.Fatalf("%s: %d wakes, want %d: %q", what, len(got), want, got)
	}
}

// A completion with background tasks running, then the thread's own next
// turn end within the hour: one wake, for the second, with its output.
// The first spends no turn of the supervisor.
func TestBackgroundTurnEndWakesAtTheNext(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	defer stop()
	turns := wt.turns(t, sv.ID)
	w, pos := wt.worker(t, owner, "c1", "a", "background 2s\nreply started a long task")
	wt.read(t, pos)
	wt.noWake(t, sv.ID, 2*time.Minute, 0, "after the turn end with a background task")
	if n := wt.turns(t, sv.ID); n != turns {
		t.Fatalf("the supervisor ran %d turns, want still %d", n, turns)
	}
	wt.read(t, wt.ended(t, w, 2))
	got := wt.wokenN(t, owner, sv.ID, 1)
	if d := got[0]; !strings.Contains(d, fmt.Sprintf("\nThread %d: ", w)) || !strings.Contains(d, "Background tasks: 0.") ||
		!strings.Contains(d, "> background done") || strings.Contains(d, staleLine) {
		t.Fatalf("the wake:\n%s", d)
	}
	wt.noWake(t, sv.ID, 2*time.Hour, 1, "after the wake")
}

// No turn within the hour: one wake at the hour, saying the background
// work has not come back; none before it, none after.
func TestBackgroundTurnEndWakesAfterAnHour(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	defer stop()
	w, pos := wt.worker(t, owner, "c1", "a", "background 10h\nreply started a long task")
	wt.read(t, pos)
	wt.noWake(t, sv.ID, 59*time.Minute, 0, "within the hour")
	wt.steps(90 * time.Second)
	got := wt.wokenN(t, owner, sv.ID, 1)
	if d := got[0]; !strings.Contains(d, fmt.Sprintf("\nThread %d: ", w)) || !strings.Contains(d, staleLine) || !strings.Contains(d, "Background tasks: 1.") {
		t.Fatalf("the wake at the hour:\n%s", d)
	}
	wt.noWake(t, sv.ID, 3*time.Hour, 1, "after the wake at the hour")
}

// A failed or an interrupted turn end with a background task running
// wakes the supervisor at once.
func TestBackgroundFailureWakesAtOnce(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	defer stop()
	_, pos := wt.worker(t, owner, "c1", "f", "background 10h\nfail api_error")
	wt.read(t, pos)
	if d := wt.wokenN(t, owner, sv.ID, 1)[0]; !strings.Contains(d, "failed (api_error). Background tasks: 1.") || strings.Contains(d, staleLine) {
		t.Fatalf("the failure's wake:\n%s", d)
	}

	ctx := context.Background()
	id, err := owner.Start(ctx, control.StartRequest{Key: "i", Scope: "c1", Thread: "t-i", Repo: wt.repo,
		Input: control.Input{Text: "background 10h\ntool wait 30s"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the tool", 15*time.Second, func() bool {
		var n int
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'tool_started'`, int64(id)).Scan(&n)
		return n == 1
	})
	if err := owner.Stop(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	wt.read(t, wt.ended(t, id, 1))
	if d := wt.wokenN(t, owner, sv.ID, 2)[1]; !strings.Contains(d, fmt.Sprintf("\nThread %d: ", id)) || !strings.Contains(d, "Background tasks: 1.") || strings.Contains(d, staleLine) {
		t.Fatalf("the interrupted turn's wake:\n%s", d)
	}
}

// A restart between the two turn ends gives one wake, for the second; a
// restart after the hour passed gives one wake, at the hour, and a restart
// after that none more.
func TestBackgroundTurnEndAcrossRestarts(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	w, pos := wt.worker(t, owner, "c1", "a", "background 4s\nreply started a long task")
	wt.read(t, pos)
	stop()
	stop, owner, sv = wt.start(t)
	wt.read(t, wt.ended(t, w, 2))
	if d := wt.wokenN(t, owner, sv.ID, 1)[0]; !strings.Contains(d, "> background done") || strings.Contains(d, staleLine) {
		t.Fatalf("the wake after a restart between the turn ends:\n%s", d)
	}
	wt.noWake(t, sv.ID, 2*time.Hour, 1, "after it")

	// Another thread's wake after the turn end moves the waker's stored
	// position past it, so after the restart the event log alone tells
	// that it still waits.
	x, pos := wt.worker(t, owner, "c1", "x", "background 10h\nreply started another")
	wt.read(t, pos)
	_, pos = wt.worker(t, owner, "c1", "y", "reply other")
	wt.read(t, pos)
	if d := wt.wokenN(t, owner, sv.ID, 2)[1]; strings.Contains(d, fmt.Sprintf("\nThread %d: ", x)) {
		t.Fatalf("the other thread's wake names the waiting one:\n%s", d)
	}
	stop()
	wt.steps(61 * time.Minute) // the daemon is down at the hour
	stop, owner, sv = wt.start(t)
	got := wt.wokenN(t, owner, sv.ID, 3)
	if d := got[2]; !strings.Contains(d, fmt.Sprintf("\nThread %d: ", x)) || !strings.Contains(d, staleLine) {
		t.Fatalf("the wake after a restart past the hour:\n%s", d)
	}
	stop()
	stop, _, sv = wt.start(t)
	defer stop()
	wt.noWake(t, sv.ID, 2*time.Hour, 3, "after another restart")
}

// The supervisor's send to a thread whose turn then ends with a background
// task running is no cycle of the loop guard: with loop_cycles 1, a wake
// of another thread in between, and the thread's own next turn end, that
// end still wakes the supervisor, and nobody is told of a loop.
func TestBackgroundTurnEndIsNoLoopCycle(t *testing.T) {
	var (
		mu    sync.Mutex
		issue func(string) (string, error)
	)
	supervisorHook = func(i func(string) (string, error), _ func(string)) { mu.Lock(); issue = i; mu.Unlock() }
	t.Cleanup(func() { supervisorHook = nil })
	wt := newWakeTest(t, budget(0, 0, 0), autonomy("act"), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["loop_cycles"] = 1
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	w, pos := wt.worker(t, owner, "c1", "w", "reply w")
	wt.read(t, pos)
	wt.wokenN(t, owner, sv.ID, 1)
	mu.Lock()
	svc := svConn(t, wt.r.root, issue)
	mu.Unlock()
	if err := svc.Send(ctx, w, "sv-1", control.Input{Text: "background 6s\nreply waiting on it"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.read(t, wt.ended(t, w, 2))
	_, pos = wt.worker(t, owner, "c1", "x", "reply other")
	wt.read(t, pos)
	if d := wt.wokenN(t, owner, sv.ID, 2)[1]; strings.Contains(d, fmt.Sprintf("\nThread %d: ", w)) {
		t.Fatalf("the other thread's wake names the waiting one:\n%s", d)
	}
	wt.read(t, wt.ended(t, w, 3))
	if d := wt.wokenN(t, owner, sv.ID, 3)[2]; !strings.Contains(d, fmt.Sprintf("\nThread %d: ", w)) {
		t.Fatalf("the thread's next turn end did not wake the supervisor:\n%s", d)
	}
	if n := wt.loopNotices(sv); len(n) != 0 {
		t.Fatalf("loop notices %q", n)
	}
}
