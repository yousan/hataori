package supervisor

import (
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
