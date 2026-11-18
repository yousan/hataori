package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

func limitsEvent(at time.Time, used int) agent.Event {
	return agent.Event{Kind: agent.Limits, Limits: &agent.LimitReport{At: at, Every: 5 * time.Minute,
		Windows: []agent.LimitWindow{{Name: "seven_day", Minutes: 10080, Used: used, Resets: time.Unix(1795161600, 0)}}}}
}

// Limits has one entry per agent that reported, with the newest report of
// any of its sessions; nothing for an agent that never did. A report
// changes no thread.
func TestLimitsKeepTheNewestPerAgent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	if ls, err := f.owner.Limits(ctx); err != nil || len(ls) != 0 {
		t.Fatalf("before any report: %+v, %v", ls, err)
	}
	id, sid := f.running(t, "t1")
	before := f.info(t, f.owner, id)
	at := time.UnixMilli(1794980000000)
	f.agentEvent(t, sid, 2, limitsEvent(at, 21))
	ls, err := f.owner.Limits(ctx)
	if err != nil || len(ls) != 1 {
		t.Fatalf("%+v, %v", ls, err)
	}
	l := ls[0]
	if l.Agent != "fake" || !l.At.Equal(at) || l.Every != 300 || len(l.Windows) != 1 || l.Windows[0].Used != 21 ||
		l.Windows[0].Minutes != 10080 || l.Windows[0].Resets == nil || !l.Windows[0].Resets.Equal(time.Unix(1795161600, 0)) {
		t.Fatalf("entry %+v", l)
	}
	if after := f.info(t, f.owner, id); after.State != before.State || after.Turns != before.Turns {
		t.Fatalf("the thread changed: %+v, then %+v", before, after)
	}

	// Another session of the same agent reports an older value: kept is
	// the newer. Then a newer one replaces it.
	_, other := f.running(t, "t2")
	f.agentEvent(t, other, 2, limitsEvent(at.Add(-time.Minute), 30))
	if ls, _ := f.owner.Limits(ctx); ls[0].Windows[0].Used != 21 {
		t.Fatalf("an older report replaced a newer one: %+v", ls)
	}
	f.agentEvent(t, other, 3, limitsEvent(at.Add(time.Minute), 22))
	if ls, _ := f.owner.Limits(ctx); len(ls) != 1 || ls[0].Windows[0].Used != 22 {
		t.Fatalf("a newer report: %+v", ls)
	}

	// It needs OpRead, and no scope.
	none := f.core.As(control.Caller{Principal: "x", Scopes: []string{"s"}})
	if _, err := none.Limits(ctx); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("without OpRead: %v", err)
	}
	reader := f.core.As(control.Caller{Principal: "x", May: control.OpRead})
	if ls, err := reader.Limits(ctx); err != nil || len(ls) != 1 {
		t.Fatalf("with OpRead in no scope: %+v, %v", ls, err)
	}
}
