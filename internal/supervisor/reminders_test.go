package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
)

// at is a Clock that stands.
type at time.Time

func (a at) Now() time.Time                     { return time.Time(a) }
func (at) After(time.Duration) <-chan time.Time { return nil }

func remindStore(t *testing.T) *core.Core {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &core.Core{Store: st, Log: slog.New(slog.DiscardHandler)}
}

// A reminder is open until a stored wake carries it, again when that wake
// is dropped, and given once that wake was; a wake made with one that was
// cancelled meanwhile is not stored. A reminder of another scope is not
// seen, and a number is used once.
func TestRemindersAndTheirWake(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	m := NewReminders(c, "c1", 10, c.Log)
	m.Clock = at(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	other := NewReminders(c, "c2", 10, c.Log)
	set := func(m *Reminders) int64 {
		t.Helper()
		a, err := m.Remind(ctx, control.RemindRequest{Delay: 600, Note: "look"})
		if err != nil {
			t.Fatal(err)
		}
		return a.Number
	}
	one, two, elsewhere := set(m), set(m), set(other)
	open := func(want ...int64) {
		t.Helper()
		rs, err := m.open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, r := range rs {
			got = append(got, r.Number)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("open: %v, want %v", got, want)
		}
	}
	r := &waking{w: &Waker{Core: c, Scope: "c1", Reminders: m}}
	do := func(f func(*store.Tx) error) error { return c.Store.Update(ctx, f) }
	open(one, two)
	if err := do(r.carriesReminders(ctx, "k1", []Reminder{{Number: one}, {Number: two}})); err != nil {
		t.Fatal(err)
	}
	open()
	if _, err := m.Remind(ctx, control.RemindRequest{Cancel: one}); err == nil {
		t.Fatal("cancelled a reminder a stored wake carries")
	}
	if _, err := m.Remind(ctx, control.RemindRequest{Cancel: elsewhere}); err == nil {
		t.Fatal("cancelled a reminder of another scope")
	}
	if err := do(r.dropsReminders(ctx)); err != nil { // refused as busy
		t.Fatal(err)
	}
	open(one, two)
	if _, err := m.Remind(ctx, control.RemindRequest{Cancel: one}); err != nil {
		t.Fatal(err)
	}
	if err := do(r.carriesReminders(ctx, "k2", []Reminder{{Number: one}, {Number: two}})); !errors.Is(err, errReminder) {
		t.Fatalf("storing a wake whose reminder was cancelled since it was made: %v", err)
	}
	open(two)
	if err := do(r.carriesReminders(ctx, "k2", []Reminder{{Number: two}})); err != nil {
		t.Fatal(err)
	}
	if err := do(r.renamesReminders(ctx, "k2", "k3")); err != nil { // for a replacing thread
		t.Fatal(err)
	}
	if err := do(r.clearsReminders(ctx, "k3")); err != nil {
		t.Fatal(err)
	}
	if err := do(r.dropsReminders(ctx)); err != nil { // what comes later
		t.Fatal(err)
	}
	open()
	if n := set(m); n != 4 {
		t.Fatalf("the next reminder is %d, want 4", n)
	}
	if n, err := remindersSet(ctx, c, "c1", m.now().Add(-time.Minute), m.now().Add(time.Minute)); err != nil || n != 3 {
		t.Fatalf("%d reminders set, %v; want 3, the cancelled one too", n, err)
	}
}

// A wake of reminders alone gave no turn end, so it is no cycle; one with a
// digest is.
func TestReminderWakeIsNoCycle(t *testing.T) {
	for key, want := range map[string]int{ReminderKey(1, 20, 3): 0, WakeKey(1, 20): 1} {
		c := remindStore(t)
		g := &LoopGuard{Core: c, Scope: "c1", Cycles: 3}
		for _, q := range []string{
			`INSERT INTO threads (id, scope, key, started_by, state, confined, kind, created_at, updated_at) VALUES (1, 'c1', 'sv', 'p', 'idle', 1, 'supervisor', 0, 0), (2, 'c1', 'w', 'p', 'idle', 0, 'ordinary', 0, 0)`,
			`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
				(10, 'c1', '', 2, 0, 'turn_started', '{"by":"caller","from":"supervisor:c1","key":"s","session":2,"turn":5}'),
				(11, 'c1', '', 2, 0, 'turn_ended', '{"by":"caller","from":"supervisor:c1","outcome":"completion","background":0,"session":2,"turn":5}')`,
			`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
				(21, 'c1', '', 1, 0, 'turn_started', '{"by":"caller","key":"` + key + `","session":1,"turn":6}')`,
		} {
			if _, err := c.Store.DB.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		if n, _, err := g.Count(context.Background(), 2); err != nil || n != want {
			t.Errorf("after a wake under %s: %d cycles, %v; want %d", key, n, err, want)
		}
	}
}
