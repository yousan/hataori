package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

// fixedClock is a clock a test sets.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fixedClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func usage(in, out, cr, cw, used, size int64) agent.Event {
	return agent.Event{Kind: agent.Usage, Usage: &agent.UsageCount{Input: in, Output: out, CacheRead: cr, CacheWrite: cw,
		ContextUsed: used, ContextSize: size}}
}

// A thread whose agent never reported usage has no tokens and no context;
// then tokens.turn is the last turn's total, tokens.today the sum of the
// turns, and context the last one reported.
func TestThreadTokensAndContext(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	if got := f.info(t, f.owner, id); got.Tokens != nil || got.Context != nil {
		t.Fatalf("before any usage: tokens %+v, context %+v", got.Tokens, got.Context)
	}
	// Each report is the turn's total so far.
	f.agentEvent(t, sid, 2, usage(10, 5, 0, 0, 1000, 200000))
	f.agentEvent(t, sid, 3, usage(30, 20, 100, 50, 1200, 200000)) // 200
	f.agentEvent(t, sid, 4, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	if err := f.owner.Send(ctx, id, "second", control.Input{Text: "again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 5, agent.Event{Kind: agent.TurnStarted})
	f.agentEvent(t, sid, 6, usage(40, 30, 0, 0, 1300, 200000)) // 70
	f.agentEvent(t, sid, 7, usage(1, 1, 0, 0, 0, 0))           // went down, and no context: adds nothing
	if got := f.info(t, f.owner, id); got.Tokens == nil || *got.Tokens != (control.TokenCount{Turn: 70, Today: 270}) {
		t.Fatalf("after a count that went down: %+v", got.Tokens)
	}
	f.agentEvent(t, sid, 8, usage(50, 30, 0, 0, 0, 0)) // 80
	f.agentEvent(t, sid, 9, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	// Reports outside a turn count toward no turn; their context is kept.
	f.agentEvent(t, sid, 10, usage(100, 0, 0, 0, 0, 0))
	f.agentEvent(t, sid, 11, usage(150, 0, 0, 0, 1400, 200000))

	got := f.info(t, f.owner, id)
	if got.Tokens == nil || *got.Tokens != (control.TokenCount{Turn: 80, Today: 280}) {
		t.Fatalf("tokens %+v, want turn 80 today 280", got.Tokens)
	}
	if got.Context == nil || *got.Context != (control.Context{Used: 1400, Size: 200000}) {
		t.Fatalf("context %+v", got.Context)
	}
	d, err := f.owner.Thread(ctx, id, 0, 0)
	if err != nil || d.Tokens == nil || *d.Tokens != *got.Tokens {
		t.Fatalf("Thread: %+v, %v", d.Tokens, err)
	}
	// Another thread's usage is not this one's.
	other, _ := f.running(t, "t2")
	if got := f.info(t, f.owner, other); got.Tokens != nil {
		t.Fatalf("the other thread: %+v", got.Tokens)
	}
}

// Usage reported before midnight in the time zone does not count toward
// the next day, even within a turn that goes on past midnight.
func TestTokensTodayStartsAtMidnight(t *testing.T) {
	zone := time.FixedZone("test", 9*60*60)
	clock := &fixedClock{t: time.Date(2026, 3, 4, 23, 58, 0, 0, zone)}
	f := newFixture(t, fakeOwner{})
	f.core.Machine.Now, f.core.Machine.Zone = clock.now, zone
	id, sid := f.running(t, "t1")
	f.agentEvent(t, sid, 2, usage(100, 0, 0, 0, 0, 0))
	if got := f.info(t, f.owner, id); got.Tokens.Today != 100 {
		t.Fatalf("before midnight: %+v", got.Tokens)
	}
	clock.set(time.Date(2026, 3, 5, 0, 0, 0, 0, zone)) // midnight, nothing reported yet
	if got := f.info(t, f.owner, id); *got.Tokens != (control.TokenCount{Turn: 100, Today: 0}) {
		t.Fatalf("at midnight: %+v", got.Tokens)
	}
	clock.set(time.Date(2026, 3, 5, 0, 1, 0, 0, zone))
	f.agentEvent(t, sid, 3, usage(130, 20, 0, 0, 0, 0)) // the same turn: 50 more
	if got := f.info(t, f.owner, id); *got.Tokens != (control.TokenCount{Turn: 150, Today: 50}) {
		t.Fatalf("after midnight: %+v", got.Tokens)
	}
	// The event's time is the clock's, so a reader can tell the days apart too.
	evs, _ := f.core.Store.Last(context.Background(), id, 1, func(e control.Event) bool { return e.Kind == "usage" })
	if len(evs) != 1 || !evs[0].Time.Equal(clock.now()) {
		t.Fatalf("usage event %+v", evs)
	}
}

// A Late usage report, of the turn that ended last and read after its end,
// counts toward that turn, and its usage event names it; a report outside
// a turn that is not Late still counts toward none (#675).
func TestLateUsageCountsTowardTheEndedTurn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	id, sid := f.running(t, "t1")
	late := usage(100, 15, 0, 0, 0, 0) // the turn's last message, read after its end: 115
	late.Late = true
	f.agentEvent(t, sid, 2, usage(10, 5, 0, 0, 0, 0)) // 15
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	f.agentEvent(t, sid, 4, late)
	f.agentEvent(t, sid, 5, usage(500, 0, 0, 0, 0, 0)) // outside a turn, not Late
	if got := f.info(t, f.owner, id); got.Tokens == nil || *got.Tokens != (control.TokenCount{Turn: 115, Today: 115}) {
		t.Fatalf("after a late report: %+v, want turn 115 today 115", got.Tokens)
	}
	var turn int64
	if err := f.core.Store.DB.QueryRow(`SELECT id FROM turns WHERE session_id = ?`, sid).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	rows, err := f.core.Store.DB.Query(`SELECT coalesce(json_extract(data, '$.turn'), 0) FROM events WHERE kind = 'usage' ORDER BY pos`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var turns []int64
	for rows.Next() {
		var n int64
		rows.Scan(&n)
		turns = append(turns, n)
	}
	if fmt.Sprint(turns) != fmt.Sprint([]int64{turn, turn, 0}) {
		t.Fatalf("the usage events' turns %v, want %d, %d and none", turns, turn, turn)
	}
	// The next turn is counted on its own.
	if err := f.owner.Send(ctx, id, "second", control.Input{Text: "again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 6, agent.Event{Kind: agent.TurnStarted})
	f.agentEvent(t, sid, 7, usage(1, 2, 0, 0, 0, 0))
	if got := f.info(t, f.owner, id); got.Tokens == nil || *got.Tokens != (control.TokenCount{Turn: 3, Today: 118}) {
		t.Fatalf("after the next turn's report: %+v, want turn 3 today 118", got.Tokens)
	}
}
