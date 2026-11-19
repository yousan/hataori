package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// loopNotices returns the loop guard's notices in the supervisor's thread.
func (wt *wakeTest) loopNotices(sv control.ThreadInfo) []string {
	var out []string
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	for _, c := range wt.r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "from now on it does not wake the supervisor") {
			out = append(out, c.Message.Text)
		}
	}
	return out
}

// settle moves the waker's clock on, a quiet period at a time, while
// there is something to wake, until ok holds; then a few more times, to
// show that nothing more comes.
func (wt *wakeTest) settle(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		if wt.clock().waiting() > 0 {
			wt.clock().advance(30 * time.Second)
		}
		time.Sleep(30 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		wt.clock().advance(30 * time.Second)
		time.Sleep(30 * time.Millisecond)
	}
}

// svConn connects as c1's supervisor, as its MCP server does, through the
// daemon now running.
func svConn(t *testing.T, root string, issue func(string) (string, error)) *wire.Client {
	t.Helper()
	token, err := issue("c1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := wire.DialToken(root, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A two-thread ping-pong, the supervisor relaying each worker's answer to
// the other, stops at 3 cycles on both threads, and a person is told once
// for each; a third thread still wakes the supervisor. After a restart the
// count holds and nobody is told again. A person's input in a worker
// thread lets it wake the supervisor again, and so does a person's
// message in the supervisor's thread.
func TestLoopGuard(t *testing.T) {
	var (
		mu    sync.Mutex
		issue func(string) (string, error)
	)
	supervisorHook = func(i func(string) (string, error), _ func(string)) { mu.Lock(); issue = i; mu.Unlock() }
	t.Cleanup(func() { supervisorHook = nil })
	wt := newWakeTest(t, budget(0, 0, 0), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "relay 2 3"
	})
	stop, owner, sv := wt.start(t)
	if sv.ID != 1 {
		t.Fatalf("the supervisor's thread is %d; the relay is set up for 1", sv.ID)
	}
	ctx := context.Background()
	a, _ := wt.worker(t, owner, "c1", "a", "reply ping")
	b, first := wt.worker(t, owner, "c1", "b", "reply pong")
	if a != 2 || b != 3 {
		t.Fatalf("the workers are %d and %d; the relay is set up for 2 and 3", a, b)
	}
	// Both first turn ends are read before the quiet period is moved on, so
	// the first wake names both. Otherwise one of them can fold into the
	// wake of its first relayed turn, which is then its first appearance and
	// its first cycle at once, and it is named in 3 wakes, not 4 (#450).
	wt.read(t, first)

	// Each thread is named in the first wake and in three more, one per
	// cycle, the last of which tells a person; after that, the turn ends the
	// supervisor's relays make wake nothing. (Whether the two threads share
	// a later wake depends on when their relayed turns end.)
	wt.settle(t, "the guard to stop both threads", func() bool { return len(wt.loopNotices(sv)) >= 2 })
	all := wt.wakes(t, sv.ID)
	named := func(ds []string, id control.ThreadID) int {
		n := 0
		for _, d := range ds {
			if strings.Contains(d, fmt.Sprintf("\nThread %d: ", id)) {
				n++
			}
		}
		return n
	}
	if na, nb := named(all, a), named(all, b); na != 4 || nb != 4 {
		t.Fatalf("thread %d is named in %d wakes and %d in %d; want 4 each: %q", a, na, b, nb, all)
	}
	before := len(all)
	notices := wt.loopNotices(sv)
	if len(notices) != 2 {
		t.Fatalf("%d loop notices, want one for each thread: %q", len(notices), notices)
	}
	for _, id := range []control.ThreadID{a, b} {
		if n := strings.Count(strings.Join(notices, "\n"), fmt.Sprintf("Thread #%d went back and forth with the supervisor 3 times", id)); n != 1 {
			t.Fatalf("thread %d is named in %d notices: %q", id, n, notices)
		}
	}

	// A third thread still wakes it, and the digest names only that one.
	c, _ := wt.worker(t, owner, "c1", "c", "reply third")
	wt.settle(t, "the third thread's wake", func() bool { return len(wt.wakes(t, sv.ID)) >= before+1 })
	got := wt.woken(t, owner, sv.ID, before+1)
	if last := got[before]; !strings.Contains(last, fmt.Sprintf("\nThread %d: ", c)) || strings.Contains(last, fmt.Sprintf("Thread %d:", a)) ||
		strings.Contains(last, fmt.Sprintf("Thread %d:", b)) {
		t.Fatalf("the third thread's wake:\n%s", last)
	}
	stop()

	// After a restart the count holds: the supervisor's send ends a turn of
	// a that wakes nothing, and nobody is told again.
	stop, owner, _ = wt.start(t)
	defer stop()
	mu.Lock()
	svc := svConn(t, wt.r.root, issue)
	mu.Unlock()
	endsOf := func(id control.ThreadID) int { return len(threadEvents(t, owner, id, "turn_ended")) }
	n := endsOf(a)
	if err := svc.Send(ctx, a, "poke-1", control.Input{Text: "reply poked"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	pos := wt.ended(t, a, n+1)
	wt.read(t, pos)
	wt.settle(t, "the waker to read it", func() bool { return true })
	if got := wt.wakes(t, sv.ID); len(got) != before+1 {
		t.Fatalf("%d wakes after a restart and the supervisor's send to %d; want still %d", len(got), a, before+1)
	}
	if n := len(wt.loopNotices(sv)); n != 2 {
		t.Fatalf("%d loop notices after the restart, want still 2", n)
	}

	// A person's input in b: b wakes the supervisor again. (Its relay to a
	// ends a turn of a, which still wakes nothing.)
	n, na := endsOf(b), endsOf(a)
	if err := owner.Send(ctx, b, "person-b", control.Input{Text: "reply a person was here"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.ended(t, b, n+1)
	wt.settle(t, "b's wake", func() bool { return len(wt.wakes(t, sv.ID)) >= before+2 })
	got = wt.woken(t, owner, sv.ID, before+2)
	if last := got[before+1]; !strings.Contains(last, fmt.Sprintf("\nThread %d: ", b)) || strings.Contains(last, fmt.Sprintf("Thread %d:", a)) {
		t.Fatalf("the wake after a person's input in %d:\n%s", b, last)
	}
	if n := len(wt.loopNotices(sv)); n != 2 {
		t.Fatalf("%d loop notices, want still 2", n)
	}
	// That wake makes the supervisor relay to a. The relayed turn of a ends,
	// and the waker reads it, while a is still stopped: after the reset below
	// it would wake the supervisor itself (#450).
	wt.read(t, wt.ended(t, a, na+1))
	wt.settle(t, "the relay to a", func() bool { return true })
	if got := wt.wakes(t, sv.ID); len(got) != before+2 {
		t.Fatalf("%d wakes after the relay to %d, which is stopped; want still %d", len(got), a, before+2)
	}

	// A person's message in the supervisor's thread: a wakes it again too.
	sendAndRead(t, owner, sv.ID, "person-sv", "reply looked at it")
	if err := svc.Send(ctx, a, "poke-2", control.Input{Text: "reply poked again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.settle(t, "a's wake", func() bool { return len(wt.wakes(t, sv.ID)) >= before+3 })
	got = wt.wakes(t, sv.ID)
	if !strings.Contains(got[before+2], fmt.Sprintf("\nThread %d: ", a)) {
		t.Fatalf("the wake after a person's message in the supervisor's thread:\n%s", got[before+2])
	}
}

// A person is told when the wake that makes the last cycle is accepted,
// before any turn end of the thread is dropped; the next one is dropped
// and tells nobody again.
func TestLoopGuardTellsAtTheLastCycle(t *testing.T) {
	var (
		mu    sync.Mutex
		issue func(string) (string, error)
	)
	supervisorHook = func(i func(string) (string, error), _ func(string)) { mu.Lock(); issue = i; mu.Unlock() }
	t.Cleanup(func() { supervisorHook = nil })
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	defer stop()
	ctx := context.Background()
	w, pos := wt.worker(t, owner, "c1", "w", "reply w")
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1)
	mu.Lock()
	svc := svConn(t, wt.r.root, issue)
	mu.Unlock()
	// The supervisor sends, the turn ends, and a wake tells it: one cycle.
	cycle := func(i int) {
		t.Helper()
		if err := svc.Send(ctx, w, fmt.Sprintf("sv-%d", i), control.Input{Text: fmt.Sprintf("reply round %d", i)}, control.Refuse); err != nil {
			t.Fatal(err)
		}
		wt.read(t, wt.ended(t, w, i+1))
	}
	for i := 1; i <= 3; i++ {
		if n := len(wt.loopNotices(sv)); n != 0 {
			t.Fatalf("a loop notice before cycle %d: %q", i, wt.loopNotices(sv))
		}
		cycle(i)
		wt.quiet(t)
		wt.woken(t, owner, sv.ID, 1+i)
	}
	eventually(t, "the notice at the last cycle", 10*time.Second, func() bool { return len(wt.loopNotices(sv)) == 1 })
	if got := wt.loopNotices(sv)[0]; !strings.Contains(got, fmt.Sprintf("Thread #%d went back and forth with the supervisor 3 times", w)) {
		t.Fatalf("the notice: %s", got)
	}
	cycle(4)
	wt.settle(t, "the waker", func() bool { return true })
	if got := wt.wakes(t, sv.ID); len(got) != 4 {
		t.Fatalf("%d wakes after the fourth round, want still 4", len(got))
	}
	if n := len(wt.loopNotices(sv)); n != 1 {
		t.Fatalf("%d loop notices, want 1", n)
	}
}
