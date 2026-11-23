package supervisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// The line of a wake answered in a conversation of its own.
func TestWakeAnsweredLine(t *testing.T) {
	at := time.Date(2026, 11, 24, 9, 5, 0, 0, time.UTC)
	head := "hataori, meanwhile: the wake of 2026-11-24 09:05 UTC"
	for _, c := range []struct {
		name string
		w    WakeAnswered
		want string
	}{
		{"an answer", WakeAnswered{Threads: []control.ThreadID{2}, Answer: "\nLooked at it.\nMore."},
			head + ` on thread 2 was answered in a conversation of its own; its answer began: "Looked at it.".`},
		{"three threads and decisions", WakeAnswered{Threads: []control.ThreadID{2, 3, 5}, Decided: true, Answer: "ok"},
			head + ` on threads 2, 3 and 5 and decisions of your requests was answered in a conversation of its own; its answer began: "ok".`},
		{"a thread and reminders", WakeAnswered{Threads: []control.ThreadID{2}, Reminders: []int64{3, 4}, Answer: "ok"},
			head + ` on thread 2 and reminder 3 and reminder 4 was answered in a conversation of its own; its answer began: "ok".`},
		{"quiet", WakeAnswered{Answer: "(quiet)\n"}, head + ` was answered in a conversation of its own; it answered (quiet), and nothing was posted.`},
		{"no answer", WakeAnswered{}, head + ` was answered in a conversation of its own; it gave no answer.`},
		{"a failure", WakeAnswered{Failed: true, Reason: "crashed", Answer: "partial"}, head + ` was answered in a conversation of its own; its turn failed (crashed).`},
		{"a long answer", WakeAnswered{Answer: strings.Repeat("あ", 100)},
			head + ` was answered in a conversation of its own; its answer began: "` + strings.Repeat("あ", 66) + `…".`},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.w.At = at
			if got := c.w.Line(); got != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// A wake whose conversation could not be started is told to a person, once
// per state event; no other change of state is.
func TestWakeNotStartedIsTold(t *testing.T) {
	var told []string
	r := &waking{w: &Waker{Tell: func(_ context.Context, id control.ThreadID, key, text string) error {
		told = append(told, key+": "+text)
		return nil
	}}}
	for _, d := range []map[string]string{
		{"from": "creating", "to": "asleep", "reason": "start: no"},
		{"from": "creating", "to": "idle"},
		{"from": "idle", "to": "asleep", "reason": "idle"},
		{"from": "creating", "to": "failed", "reason": "start: no"},
	} {
		b, _ := json.Marshal(d)
		if err := r.notStarted(context.Background(), control.Event{Pos: 7, Kind: "state", Data: b}); err != nil {
			t.Fatal(err)
		}
	}
	want := "hataori-wake-failed-7: A wake could not start its agent, so it was not read. Reason: start: no."
	if len(told) != 1 || told[0] != want {
		t.Fatalf("told %q, want %q", told, want)
	}
}
