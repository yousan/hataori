package session

import (
	"bufio"
	"encoding/json"
	"errors"
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
			// The original excerpt omits delivery kind. Supply the adapter's
			// asynchronous classification explicitly; approval/choices cannot
			// distinguish an asynchronous question from a blocking question.
			e.Request = &agent.RequestInfo{ID: "fixture-question", Approval: captured.Data.Approval, Choices: captured.Data.Choices, NonBlocking: true}
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

func TestClarificationCompletedBeforeAnswer(t *testing.T) {
	m, id := newMachine(t, control.Idle)
	feed(t, m, id, Input{Kind: Send, Key: "first"},
		ag(agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "question", NonBlocking: true}}),
		ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Completed}))
	var state string
	var open int
	if err := m.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM requests WHERE session_id = ? AND state = 'open'`, id).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if state != string(control.Idle) || open != 1 {
		t.Fatalf("completed turn with unanswered clarification: state=%s open=%d", state, open)
	}
	feed(t, m, id, Input{Kind: Answer, Principal: "person", Request: "question", Text: "answer"},
		ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Completed}))
	if got := ended(t, m); len(got) != 2 || got[0]["outcome"] != Completion || got[1]["outcome"] != Completion {
		t.Fatalf("answer after completion: ends=%v", got)
	}
}

func TestClarificationWithdrawalKeepsTurnBoundary(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "completed"}[completed], func(t *testing.T) {
			m, id := newMachine(t, control.Idle)
			feed(t, m, id, Input{Kind: Send, Key: "first"},
				ag(agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "question", NonBlocking: true}}))
			wantState, wantEnds := control.Running, 0
			if completed {
				feed(t, m, id, ag(agent.Event{Kind: agent.TurnEnded}))
				wantState, wantEnds = control.Idle, 1
			}
			feed(t, m, id, ag(agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "question"}}))
			var state, requestState string
			var turns int
			if err := m.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if err := m.Store.DB.QueryRow(`SELECT state FROM requests WHERE session_id = ? AND request = 'question'`, id).Scan(&requestState); err != nil {
				t.Fatal(err)
			}
			if err := m.Store.DB.QueryRow(`SELECT count(*) FROM turns WHERE session_id = ?`, id).Scan(&turns); err != nil {
				t.Fatal(err)
			}
			if state != string(wantState) || requestState != RequestWithdrawn || turns != 1 || len(ended(t, m)) != wantEnds {
				t.Fatalf("withdrawal changed turn boundary: state=%s request=%s turns=%d", state, requestState, turns)
			}
		})
	}
}

func TestClarificationAnswerNeedsOpenRequest(t *testing.T) {
	for _, state := range []control.State{control.Idle, control.Running} {
		for _, request := range []string{"missing", ""} {
			m, id := newMachine(t, control.Idle)
			if state == control.Running {
				feed(t, m, id, Input{Kind: Send, Key: "first"})
			}
			before := len(events(t, m))
			err := m.Apply(t.Context(), id, Input{Kind: Answer, Request: request, Text: "answer"})
			if !errors.Is(err, ErrRefused) || len(events(t, m)) != before {
				t.Fatalf("answer without an open request in %s: %v", state, err)
			}
		}
	}
}

func TestClarificationFollowedByBlockingRequest(t *testing.T) {
	for _, approval := range []bool{false, true} {
		m, id := newMachine(t, control.Idle)
		feed(t, m, id, Input{Kind: Send, Key: "first"},
			ag(agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "async", NonBlocking: true}}),
			ag(agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "async"}}),
			// Even an approval incorrectly marked NonBlocking must block.
			ag(agent.Event{Kind: agent.Request, Background: 2, Request: &agent.RequestInfo{ID: "blocking", Approval: approval, NonBlocking: approval}}))
		got := ended(t, m)
		if len(got) != 1 || got[0]["outcome"] != Blocked || got[0]["background"] != float64(2) {
			t.Fatalf("blocking request approval=%t: ends=%v", approval, got)
		}
		feed(t, m, id, Input{Kind: Answer, Request: "blocking", Text: "answer"},
			ag(agent.Event{Kind: agent.TurnEnded}))
		if got := ended(t, m); len(got) != 2 || got[1]["outcome"] != Completion {
			t.Fatalf("blocking request answer approval=%t: ends=%v", approval, got)
		}
	}
}
