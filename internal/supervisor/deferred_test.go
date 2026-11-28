package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// A record-only queue peer exercises the actual limiter and durable accounting
// without a CLI, host process, model turn or wake.
type deferredBudgetPeer struct {
	control.API
	c        *core.Core
	now      time.Time
	requests []control.DeferredSendRequest
}

func (p *deferredBudgetPeer) DeferredSend(ctx context.Context, r control.DeferredSendRequest) (control.DeferredSend, error) {
	p.requests = append(p.requests, r)
	if r.Action == "inspect" || r.Action == "cancel" {
		return control.DeferredSend{Number: r.Number, State: "cancelled"}, nil
	}
	var n int64
	var raw string
	err := p.c.Store.DB.QueryRowContext(ctx, `SELECT id,data FROM deferred_sends WHERE issuer=1 AND key=?`, r.Key).Scan(&n, &raw)
	if err == nil {
		var saved struct{ Text string }
		json.Unmarshal([]byte(raw), &saved)
		if saved.Text != r.Text {
			return control.DeferredSend{}, control.ErrForbidden
		}
		return control.DeferredSend{Number: n, State: "queued", Target: r.Target}, nil
	}
	if r.Action == "find" {
		return control.DeferredSend{State: "absent"}, nil
	}
	data, _ := json.Marshal(map[string]any{"From": ID("c1"), "Text": r.Text})
	err = p.c.Store.DB.QueryRowContext(ctx, `INSERT INTO deferred_sends(scope,issuer,key,target,session,state,data,created_at,updated_at) VALUES('c1',1,?,2,2,'cancelled',?,?,?) RETURNING id`, r.Key, string(data), p.now.UnixMilli(), p.now.UnixMilli()).Scan(&n)
	return control.DeferredSend{Number: n, State: "queued", Target: r.Target}, err
}

func TestDeferredBudgetRetryAtLimitAndRestart(t *testing.T) {
	ctx := context.Background()
	c := remindStore(t)
	now := time.Now().UTC()
	for i := 1; i <= 2; i++ {
		if _, e := c.Store.DB.Exec(`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at) VALUES(?,'c1',?,'owner','idle',0,0)`, i, fmt.Sprint(i)); e != nil {
			t.Fatal(e)
		}
		if _, e := c.Store.DB.Exec(`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(?,?,'fake','idle',0,0)`, i, i); e != nil {
			t.Fatal(e)
		}
	}
	p := &deferredBudgetPeer{c: c, now: now}
	newBudget := func() *Budget {
		return &Budget{Core: c, Scope: "c1", Clock: at(now), Zone: time.UTC, Log: c.Log, Limits: Limits{Sends: 1, Tokens: 1}}
	}
	b := newBudget()
	r := control.DeferredSendRequest{Action: "queue", Target: 2, Key: "stable", Text: "follow up"}
	got, e := b.API(p).DeferredSend(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	for range 2 {
		b = newBudget() // all in-memory granted state is lost
		if u, e := b.Used(ctx); e != nil || u.Sends != 1 {
			t.Fatalf("durable reservation: %+v %v", u, e)
		}
		again, e := b.API(p).DeferredSend(ctx, r)
		if e != nil || again.Number != got.Number {
			t.Fatalf("lost reply retry at limit: %+v %v", again, e)
		}
	}
	other := r
	other.Key = "another"
	if _, e = b.API(p).DeferredSend(ctx, other); !errors.Is(e, control.ErrForbidden) {
		t.Fatal("new queue bypassed send budget", e)
	}
	// Linked proposal and eventual input must not count the same grant again.
	for _, ev := range []struct{ kind, raw string }{
		{"proposal_made", `{"from":"supervisor:c1","deferred":1}`},
		{"input", `{"from":"supervisor:c1","via":"deferred"}`},
	} {
		if _, e = c.Store.DB.Exec(`INSERT INTO events(scope,thread,thread_id,time,kind,data) VALUES('c1','',1,?,?,?)`, now.UnixMilli(), ev.kind, ev.raw); e != nil {
			t.Fatal(e)
		}
	}
	if u, e := newBudget().Used(ctx); e != nil || u.Sends != 1 {
		t.Fatalf("queue/proposal/input double charge: %+v %v", u, e)
	}
	if _, e = b.API(p).DeferredSend(ctx, control.DeferredSendRequest{Action: "cancel", Number: got.Number}); e != nil {
		t.Fatal(e)
	}
	if u, e := newBudget().Used(ctx); e != nil || u.Sends != 1 {
		t.Fatalf("cancellation refunded a grant: %+v %v", u, e)
	}
}

func TestDeferredMCPHasSeparateExplicitTools(t *testing.T) {
	for _, name := range []string{ToolDefer, ToolDeferred, ToolCancelDeferred} {
		var found bool
		for _, tool := range tools {
			if tool.Name == name {
				found = true
				if tool.InputSchema == nil {
					t.Fatal("untyped tool", name)
				}
			}
		}
		if !found {
			t.Fatal("missing tool", name)
		}
	}
	c := remindStore(t)
	p := &deferredBudgetPeer{c: c, now: time.Now()}
	m := &MCP{Dial: func() (control.API, error) { return p, nil }, Scope: "c1"}
	if _, e := m.call(context.Background(), ToolDefer, []byte(`{"number":2,"text":"later"}`)); e == nil {
		t.Fatal("queue accepted no stable retry key")
	}
	// Inspection is never an immediate send or an answer.
	if text, e := m.call(context.Background(), ToolDeferred, []byte(`{"number":7}`)); e != nil || !strings.Contains(text, `"number":7`) {
		t.Fatal(text, e)
	}
	if len(p.requests) == 0 || p.requests[len(p.requests)-1].Action != "inspect" {
		t.Fatal(p.requests)
	}
}
