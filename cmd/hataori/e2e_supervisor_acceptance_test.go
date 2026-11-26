package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// The acceptance scenario of #275 keeps the same two workers throughout:
// an idle hour gives no input, one turn end gives one wake, and ten turn
// ends within the quiet period give one more wake.
func TestSupervisorTwoWorkersIdleThenSingleAndBurst(t *testing.T) {
	wt := newWakeTest(t)
	stop, owner, sv := wt.start(t)
	defer stop()
	a, _ := wt.worker(t, owner, "c1", "a", "reply alpha")
	b, pos := wt.worker(t, owner, "c1", "b", "reply beta")
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1)

	inputs, turns := len(wt.inputs(t, sv.ID)), wt.turns(t, sv.ID)
	wt.hour()
	time.Sleep(300 * time.Millisecond)
	if got := len(wt.inputs(t, sv.ID)); got != inputs {
		t.Fatalf("%d supervisor inputs after the idle hour, want %d", got, inputs)
	}
	if got := wt.turns(t, sv.ID); got != turns {
		t.Fatalf("%d supervisor turns after the idle hour, want %d", got, turns)
	}

	if err := owner.Send(context.Background(), a, "single", control.Input{Text: "reply single"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.read(t, wt.ended(t, a, 2))
	wt.quiet(t)
	got := wt.woken(t, owner, sv.ID, 2)
	if !strings.Contains(got[1], "a turn ended in 1 thread") || !strings.Contains(got[1], "> single\n") {
		t.Fatalf("the single turn's wake: %s", got[1])
	}
	if got := wt.turns(t, sv.ID); got != turns+1 {
		t.Fatalf("%d supervisor turns after one turn end, want %d", got, turns+1)
	}

	ends := map[control.ThreadID]int{a: 2, b: 1}
	for i := 0; i < 10; i++ {
		id := []control.ThreadID{a, b}[i%2]
		key := fmt.Sprintf("burst-%d", i)
		if err := owner.Send(context.Background(), id, key, control.Input{Text: "reply " + key}, control.Refuse); err != nil {
			t.Fatal(err)
		}
		ends[id]++
		wt.read(t, wt.ended(t, id, ends[id]))
	}
	eventually(t, "the burst's quiet timer", 10*time.Second, func() bool { return wt.clock().waiting() == 1 })
	wt.clock().advance(30*time.Second - time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if got := wt.taken(t, sv.ID); got != 2 {
		t.Fatalf("%d wakes accepted before the quiet period ended, want 2", got)
	}
	wt.clock().advance(time.Millisecond)
	got = wt.woken(t, owner, sv.ID, 3)
	for _, id := range []control.ThreadID{a, b} {
		if n := strings.Count(got[2], fmt.Sprintf("\nThread %d: ", id)); n != 1 {
			t.Fatalf("thread %d appears %d times in the burst's wake: %s", id, n, got[2])
		}
	}
	for _, text := range []string{"> burst-8\n", "> burst-9\n"} {
		if !strings.Contains(got[2], text) {
			t.Fatalf("the burst's wake lacks %q: %s", text, got[2])
		}
	}
	wt.hour()
	time.Sleep(300 * time.Millisecond)
	if got := wt.taken(t, sv.ID); got != 3 {
		t.Fatalf("%d wakes accepted after the burst settled, want 3", got)
	}
	if got := wt.turns(t, sv.ID); got != turns+2 {
		t.Fatalf("%d supervisor turns after ten more turn ends, want %d", got, turns+2)
	}
}
