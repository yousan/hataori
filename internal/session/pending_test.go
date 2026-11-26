package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

func TestLatePendingFailureDoesNotChangeAReplacedThread(t *testing.T) {
	for _, row := range []bool{false, true} {
		m, sid := newMachine(t, control.Failed)
		for _, q := range []string{
			`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (2, 1, 'fake', 'running', 0, 0)`,
			`UPDATE threads SET state = 'running'`,
		} {
			if _, err := m.Store.DB.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		if row {
			if _, err := m.Store.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data) VALUES (42, 1, '{"text":"legacy"}')`); err != nil {
				t.Fatal(err)
			}
		}
		feed(t, m, sid, ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.NotDelivered, Input: "42"}))
		var state string
		if err := m.Store.DB.QueryRow(`SELECT state FROM threads WHERE id = 1`).Scan(&state); err != nil || state != "running" {
			t.Fatalf("with pending %v: thread %q: %v", row, state, err)
		}
	}
}

func TestPendingFailureEndsItsOwnTurn(t *testing.T) {
	m, sid := newMachine(t, control.Idle)
	feed(t, m, sid, Input{Kind: Send, Key: "m1", Principal: "caller"})
	var turn int64
	if err := m.Store.DB.QueryRow(`SELECT id FROM turns WHERE ended_at IS NULL`).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	p := PendingInput{Input: control.Input{Text: "one"}, Origin: &InputOrigin{Key: "m1", Turn: turn}}
	b, _ := json.Marshal(p)
	if _, err := m.Store.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data) VALUES (42, 1, ?)`, string(b)); err != nil {
		t.Fatal(err)
	}
	feed(t, m, sid, ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.NotDelivered, Input: "42"}))
	got := ended(t, m)
	if len(got) != 1 || got[0]["reason"] != string(agent.NotDelivered) || got[0]["turn"] != float64(turn) {
		t.Fatalf("turn ends: %v", got)
	}
	if err := m.Apply(context.Background(), sid, ag(agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByAgent})); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentAndTypedTurnAttributionStaysTheSame(t *testing.T) {
	for _, tc := range []struct {
		by   agent.Starter
		want string
	}{{agent.ByAgent, ByAgent}, {agent.ByTerminal, ByTerminal}} {
		m, sid := newMachine(t, control.Idle)
		m.Typist = "typist"
		feed(t, m, sid, ag(agent.Event{Kind: agent.TurnStarted, StartedBy: tc.by, Input: "42", Text: "typed"}))
		var by, principal, from, key string
		if err := m.Store.DB.QueryRow(`SELECT started_by, principal, from_id, trigger FROM turns`).Scan(&by, &principal, &from, &key); err != nil || by != tc.want || principal != "" || from != "" || key != "" {
			t.Fatalf("%v turn %q/%q/%q/%q: %v", tc.by, by, principal, from, key, err)
		}
	}
}
