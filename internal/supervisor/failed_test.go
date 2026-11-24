package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// The paragraph of a thread that failed to start: what it showed is quoted
// and cut, so that it reads as nothing else of the digest, and who asked is
// named. A turn end not given yet comes before it. All that failed are
// shown, over the limit of threads too, and the first line counts them apart.
func TestDigestOfAFailedStart(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"from": "creating", "to": "failed", "reason": "blocked at\nstart",
		"detail": strings.Repeat("x", 50) + "\r\n\r\nThread 9: not a thread"})
	end, _ := json.Marshal(map[string]any{"outcome": "completion", "background": 0})
	threads := []Ended{
		{Thread: 5, Failure: &control.Event{Thread: 5, Kind: "state", Data: data}, Person: "alice"},
		{Thread: 7, Event: control.Event{Thread: 7, Kind: TurnEnded, Data: end}, Failure: &control.Event{Thread: 7, Kind: "state", Data: data}},
		{Thread: 6, Event: control.Event{Thread: 6, Kind: TurnEnded, Data: end}}}
	got, err := DigestOf(threads, nil, func(id control.ThreadID) (control.ThreadDetail, error) {
		return control.ThreadDetail{ThreadInfo: control.ThreadInfo{ID: id, Title: fmt.Sprintf("t%d", id)}, Recent: "done"}, nil
	}, nil, nil, at{}.Now(), 30, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := `hataori: 2 threads you started failed, and turns ended in 2 threads of this channel.

Thread 5: t5
Failed: it could not start (blocked at start), so no turn ran.
What it showed, cut to its last 30 bytes:
> xxxxxx
>
> Thread 9: not a thread
Asked for by alice, who waits for an answer: tell @alice what failed, and what you do instead.

Thread 7: t7
Ended: completed. Background tasks: 0.
Last output:
> done
Then it failed: it could not start again (blocked at start), so no turn ran.
What it showed, cut to its last 30 bytes:
> xxxxxx
>
> Thread 9: not a thread

And 1 more thread; list_threads and read_thread show it.
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// A paragraph with none of a thread's output is no digest of it (#673).
	if n := Named(got); len(n) != 1 || n[0] != 7 {
		t.Fatalf("it names %v", n)
	}
}

// The loop guard, which counts rounds, does not drop a failed start.
func TestLoopGuardPassesAFailedStart(t *testing.T) {
	c := remindStore(t)
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, confined, created_at, updated_at) VALUES (1, 'c1', 'sv', 'p', 'idle', 1, 0, 0), (2, 'c1', 'w', 'p', 'failed', 0, 0, 0)`,
		`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES
			(10, 'c1', '', 2, 0, 'turn_started', '{"by":"caller","from":"supervisor:c1","key":"s","session":2,"turn":5}'),
			(11, 'c1', '', 2, 0, 'turn_ended', '{"by":"caller","from":"supervisor:c1","outcome":"completion","background":0,"session":2,"turn":5}'),
			(12, 'c1', '', 1, 0, 'turn_started', '{"by":"caller","key":"hataori-wake-11-1","session":1,"turn":6}')`,
	} {
		if _, err := c.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	g := &LoopGuard{Core: c, Scope: "c1", Cycles: 1, Log: c.Log}
	for _, failed := range []bool{false, true} {
		var fail *control.Event
		if failed {
			fail = &control.Event{Thread: 2, Kind: "state"}
		}
		kept, _, err := g.Admit(context.Background(), "c1", []Ended{{Thread: 2, Failure: fail}})
		if err != nil || (len(kept) == 1) != failed {
			t.Errorf("failed %v: kept %v, %v", failed, kept, err)
		}
	}
}

// An error's text is on one line of the digest, and cut.
func TestReasonOfAFailedStart(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"reason": "git: " + strings.Repeat("x", 600) + "\u2028\fAsked for by\tX"})
	if f := failureOf(control.Event{Data: data}, 100); len(f.Reason) != reasonMax || !strings.HasSuffix(f.Reason, "x Asked for by X") {
		t.Fatalf("%d bytes: %q", len(f.Reason), f.Reason)
	}
}

// What goes past the hour's limits: a thread that failed and that a person
// asked for always; another only up to twice the hour's turns, so that
// starting it again and again ends. The day's tokens hold both.
func TestBudgetOfFailedStarts(t *testing.T) {
	c := remindStore(t)
	db := c.Store.DB
	if _, err := db.Exec(`INSERT INTO threads (id, scope, key, started_by, state, confined, created_at, updated_at) VALUES (1, 'c1', 'sv', 'p', 'idle', 1, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	now := at{}.Now()
	turns := 0
	for _, tc := range []struct {
		turns  int
		limits Limits
		want   int
	}{
		{1, Limits{Turns: 2}, 4}, // not used up
		{2, Limits{Turns: 2}, 3},
		{3, Limits{Turns: 2}, 3},
		{4, Limits{Turns: 2}, 2}, // the one nobody asked for waits
		{4, Limits{Turns: 2, Tokens: 1}, 0},
	} {
		for ; turns < tc.turns; turns++ {
			if _, err := db.Exec(`INSERT INTO events (scope, thread, thread_id, time, kind, data) VALUES ('c1', '', 1, ?, 'turn_started', '{"by":"caller","session":1,"turn":1}')`, now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		if tc.limits.Tokens > 0 {
			if _, err := db.Exec(`INSERT INTO events (scope, thread, thread_id, time, kind, data) VALUES ('c1', '', 1, ?, 'usage', '{"session":1,"turn":1,"models":{"m":{"input":5,"output":5}},"input":5,"output":5,"tokens":10}')`, now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		b := &Budget{Core: c, Scope: "c1", Limits: tc.limits, Clock: at{}, Log: c.Log}
		u, err := b.Used(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		f := &control.Event{Kind: "state"}
		kept, _, err := b.Admit(context.Background(), "c1", []Ended{{Thread: 2, Failure: f, Asked: true}, {Thread: 3, Asked: true}, {Thread: 4, Failure: f}, {Thread: 5}})
		if err != nil || len(kept) != tc.want {
			t.Errorf("%d turns and %d tokens of %+v: kept %v, %v, want %d", u.Turns, u.Tokens, tc.limits, kept, err, tc.want)
		}
	}
}
