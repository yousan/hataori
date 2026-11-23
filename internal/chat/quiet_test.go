package chat

import (
	"testing"
	"time"
)

// Quiet hours end where in stops holding, also when the clocks go
// forward over their end: 01:00 to 02:30 in New York on the night 02:00
// is skipped ends at 03:00, the first minute out of the span.
func TestQuietHoursEndAtAClockChange(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	q := &QuietHours{From: 60, To: 150, Zone: ny}
	at := time.Date(2026, 3, 8, 1, 30, 0, 0, ny)
	end := q.end(at)
	if !q.in(at) || q.in(end) || !q.in(end.Add(-time.Minute)) || !end.Equal(time.Date(2026, 3, 8, 3, 0, 0, 0, ny)) {
		t.Fatalf("quiet from 01:30: in %v, ends %v (in %v, a minute before %v), want 03:00 EDT",
			q.in(at), end, q.in(end), q.in(end.Add(-time.Minute)))
	}
	// Across midnight, the span ends the next day.
	q = &QuietHours{From: 22 * 60, To: 7 * 60, Zone: ny}
	if end := q.end(time.Date(2026, 6, 1, 23, 0, 0, 0, ny)); !end.Equal(time.Date(2026, 6, 2, 7, 0, 0, 0, ny)) {
		t.Fatalf("quiet from 23:00 ends %v, want 07:00 the next day", end)
	}
}
