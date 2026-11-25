package supervisor

import (
	"context"
	"fmt"
	"testing"
	"time"
	_ "time/tzdata" // the zone below, also where the system has none
)

// In the hour repeated when the clocks go back, the hour window is that
// hour, not the first one with the same wall clock time, and a limit
// reached in the first one holds nothing in the second.
func TestHourWindowInTheRepeatedHour(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC).In(ny) // 01:30 EDT
	second := first.Add(time.Hour)                                // 01:30 EST
	for _, now := range []time.Time{first, second} {
		hour, _ := windows(now)
		if d := now.Sub(hour); d != 30*time.Minute {
			t.Fatalf("at %v the hour starts at %v, %v before; want 30m", now, hour, d)
		}
	}
	l := Limits{Turns: 1}
	hour, day := windows(first)
	if _, _, until := l.over(Used{Now: second, Hour: hour, Day: day, Turns: 1}); !until.IsZero() {
		t.Fatalf("a count of the first 01:00 hour holds until %v in the second", until)
	}
}

// A token read from the cache counts a tenth, for a turn that reports once
// and for one that reports several times: each report adds what it raises
// the turn's weighted total by, once, in the day it came (#793).
func TestBudgetCountsCacheReadsATenth(t *testing.T) {
	type report struct {
		day                                  int // 0 is today
		turn                                 int
		input, output, cacheRead, cacheWrite int64
	}
	for _, tc := range []struct {
		name    string
		reports []report
		want    int64
	}{
		{"one report", []report{{0, 1, 10, 30, 1000, 20}}, 160},
		{"several reports of a turn", []report{{0, 1, 10, 30, 1000, 20}, {0, 1, 10, 30, 1005, 20}, {0, 1, 15, 40, 3009, 20}}, 375},
		{"a report again, and a lower one", []report{{0, 1, 10, 30, 1000, 20}, {0, 1, 10, 30, 1000, 20}, {0, 1, 10, 0, 1000, 20}}, 160},
		{"two turns", []report{{0, 1, 10, 30, 1000, 20}, {0, 2, 10, 30, 1000, 20}}, 320},
		{"a turn across midnight", []report{{-1, 1, 10, 30, 1000, 20}, {0, 1, 10, 30, 2000, 20}}, 100},
	} {
		c := remindStore(t)
		db := c.Store.DB
		if _, err := db.Exec(`INSERT INTO threads (id, scope, key, started_by, state, confined, created_at, updated_at) VALUES (1, 'c1', 'sv', 'p', 'idle', 1, 0, 0)`); err != nil {
			t.Fatal(err)
		}
		now := at(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		for _, r := range tc.reports {
			if _, err := db.Exec(`INSERT INTO events (scope, thread, thread_id, time, kind, data) VALUES ('c1', '', 1, ?, 'usage', ?)`,
				now.Now().AddDate(0, 0, r.day).UnixMilli(),
				fmt.Sprintf(`{"session":1,"turn":%d,"input":%d,"output":%d,"cache_read":%d,"cache_write":%d}`, r.turn, r.input, r.output, r.cacheRead, r.cacheWrite)); err != nil {
				t.Fatal(err)
			}
		}
		b := &Budget{Core: c, Scope: "c1", Clock: now, Zone: time.UTC, Log: c.Log}
		if u, err := b.Used(context.Background()); err != nil || u.Tokens != tc.want {
			t.Errorf("%s: %d tokens, %v; want %d", tc.name, u.Tokens, err, tc.want)
		}
	}
}

// The line of a wake on the budget: each limit that is on; from three
// quarters of the day's tokens on the day's end and the open reminders due
// before it (#794).
func TestBudgetLine(t *testing.T) {
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	end := day.AddDate(0, 0, 1)
	open := []Reminder{{Number: 1, Due: end.Add(-time.Minute)}, {Number: 2, Due: end}, {Number: 3, Due: end.Add(time.Hour)}}
	u := func(tokens int64) Used {
		return Used{Now: day.Add(time.Hour), Day: day, Turns: 5, Sends: 3, Tokens: tokens}
	}
	warned := func(due string) string {
		return " 75% or more of the day's tokens are used; the day ends at 2026-01-03 00:00 UTC, and " + due + " due before then."
	}
	for _, tc := range []struct {
		limits Limits
		used   Used
		open   []Reminder
		want   string
	}{
		{Limits{12, 20, 1000}, u(749), open, "5 of 12 turns this hour, 3 of 20 sends and starts this hour, 749 of 1000 tokens today."},
		{Limits{12, 20, 1000}, u(750), open, "5 of 12 turns this hour, 3 of 20 sends and starts this hour, 750 of 1000 tokens today." + warned("1 open reminder is")},
		{Limits{0, 20, 1000}, u(900), open[1:], "3 of 20 sends and starts this hour, 900 of 1000 tokens today." + warned("0 open reminders are")},
		{Limits{12, 0, 0}, u(900), open, "5 of 12 turns this hour."},
		{Limits{0, 0, 7}, u(5), nil, "5 of 7 tokens today."},
		{Limits{0, 0, 7}, u(6), []Reminder{open[0], open[0]}, "6 of 7 tokens today." + warned("2 open reminders are")},
		{Limits{}, u(900), open, ""},
	} {
		want := tc.want
		if want != "" {
			want = "\nBudget used: " + want + "\n"
		}
		if got := tc.limits.Line(tc.used, tc.open); got != want {
			t.Errorf("%+v of %+v: %q, want %q", tc.used, tc.limits, got, want)
		}
	}
}
