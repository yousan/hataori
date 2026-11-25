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
