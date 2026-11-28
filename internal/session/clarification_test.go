package session

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

// Replay the selected actual events in issue #1111, keeping their order and
// relevant fields. The fixture is the issue's already redacted excerpt, not
// an app-server capture. Request and tool identifiers below are fixture-only
// stand-ins for the omitted identifiers. State/turn events are observations
// to check, rather than inputs to the session machine.
func TestClarificationContinuingEventsKeepActiveTurn(t *testing.T) {
	m, id := newMachine(t, control.Idle)
	feed(t, m, id, Input{Kind: Send, Principal: "fixture-person", Key: "fixture-input"})
	f, err := os.Open("testdata/clarification-continuing-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var captured struct {
			Kind control.EventKind `json:"kind"`
			Data struct {
				Approval bool     `json:"approval"`
				Choices  []string `json:"choices"`
				Tool     string   `json:"tool"`
				Final    bool     `json:"final"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scan.Bytes(), &captured); err != nil {
			t.Fatal(err)
		}
		e := agent.Event{}
		switch captured.Kind {
		case "request":
			e.Kind = agent.Request
			e.Request = &agent.RequestInfo{ID: "fixture-question", Approval: captured.Data.Approval, Choices: captured.Data.Choices}
		case "tool_started":
			e.Kind, e.Tool, e.ID = agent.ToolStarted, captured.Data.Tool, "fixture-tool"
		case "tool_ended":
			e.Kind, e.Tool, e.ID = agent.ToolEnded, captured.Data.Tool, "fixture-tool"
		case "text":
			e.Kind, e.Final = agent.Text, captured.Data.Final
		default:
			continue
		}
		feed(t, m, id, ag(e))
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	var state, threadState string
	var openTurns, openQuestions int
	if err := m.Store.DB.QueryRow(`SELECT s.state, t.state FROM sessions s JOIN threads t ON t.id = s.thread_id WHERE s.id = ?`, id).Scan(&state, &threadState); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM turns WHERE session_id = ? AND ended_at IS NULL`, id).Scan(&openTurns); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM requests WHERE session_id = ? AND state = 'open'`, id).Scan(&openQuestions); err != nil {
		t.Fatal(err)
	}
	if state != string(control.Running) || threadState != string(control.Running) || openTurns != 1 {
		t.Errorf("same-session tools/progress continued before any answer or new turn: session=%s thread=%s open turns=%d; want running/running/1", state, threadState, openTurns)
	}
	if got := ended(t, m); len(got) != 0 {
		t.Errorf("active clarification emitted premature turn end: %v", got)
	}
	if openQuestions != 1 {
		t.Errorf("pending clarification disappeared: open questions=%d", openQuestions)
	}
	feed(t, m, id, Input{Kind: Answer, Principal: "fixture-person", Key: "fixture-answer", Text: "fixture answer", Request: "fixture-question"})
	feed(t, m, id, ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Completed}))
	var turns int
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM turns WHERE session_id = ?`, id).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if got := ended(t, m); turns != 1 || len(got) != 1 || got[0]["outcome"] != Completion {
		t.Errorf("clarification answer split one active turn: turns=%d ends=%v", turns, got)
	}
}
