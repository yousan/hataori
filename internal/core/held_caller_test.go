package core

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

func TestHeldMessageStartsItsCallersTurn(t *testing.T) {
	f := newFixture(t, fakeOwner{})
	f.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:sender"}}}, "owner", nil)
	id, sid := f.running(t, "t1")
	a := f.core.As(control.Caller{From: "chat:sender", Scopes: []string{"s"}, May: all})
	if err := a.Send(context.Background(), id, "held-message", control.Input{Text: "held input"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	var pending int64
	if err := f.core.Store.DB.QueryRow(`SELECT max(id) FROM pending_inputs WHERE session_id = ?`, sid).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByCaller, Input: strconv.FormatInt(pending, 10)})
	var raw string
	if err := f.core.Store.DB.QueryRow(`SELECT data FROM events WHERE kind = 'turn_started' ORDER BY pos DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"by": "caller", "key": "held-message", "principal": "owner", "from": "chat:sender"} {
		if data[k] != want {
			t.Errorf("turn_started %s = %v, want %v", k, data[k], want)
		}
	}
	var by, principal, from, key string
	if err := f.core.Store.DB.QueryRow(`SELECT started_by, principal, from_id, trigger FROM turns WHERE session_id = ? AND ended_at IS NULL`, sid).Scan(&by, &principal, &from, &key); err != nil || by != "caller" || principal != "owner" || from != "chat:sender" || key != "held-message" {
		t.Fatalf("turn row %q/%q/%q/%q: %v", by, principal, from, key, err)
	}
}
