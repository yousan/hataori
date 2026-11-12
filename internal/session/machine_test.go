package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// newMachine returns a machine with one thread and one session in state.
func newMachine(t *testing.T, state control.State) (*Machine, int64) {
	t.Helper()
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "state"), 0o700)
	s, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB.Exec(`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 'k', 'p', ?, 0, 0)`, state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'fake', ?, 0, 0)`, state); err != nil {
		t.Fatal(err)
	}
	return &Machine{Store: s}, 1
}

type logged struct {
	Kind control.EventKind
	Data map[string]any
}

func events(t *testing.T, m *Machine) []logged {
	t.Helper()
	evs, err := m.Store.Events(context.Background(), control.Filter{}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []logged
	for _, e := range evs {
		l := logged{Kind: e.Kind}
		json.Unmarshal(e.Data, &l.Data)
		out = append(out, l)
	}
	return out
}

func ended(t *testing.T, m *Machine) []map[string]any {
	var out []map[string]any
	for _, e := range events(t, m) {
		if e.Kind == KindTurnEnded {
			out = append(out, e.Data)
		}
	}
	return out
}

func feed(t *testing.T, m *Machine, id int64, ins ...Input) {
	t.Helper()
	for _, in := range ins {
		if err := m.Apply(context.Background(), id, in); err != nil {
			t.Fatalf("Apply(%s): %v", in.Kind, err)
		}
	}
}

var seq uint64

func ag(e agent.Event) Input {
	seq++
	e.Seq = seq
	return Input{Kind: AgentEvent, Agent: e}
}

func TestOneEndEventPerTurn(t *testing.T) {
	m, id := newMachine(t, control.Idle)
	// A turn a caller starts, which leaves one background task running.
	feed(t, m, id,
		Input{Kind: Send, Principal: "hook-1", Key: "m1", Text: "hi"},
		ag(agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByCaller}),
		ag(agent.Event{Kind: agent.Text, Text: "done", Final: true}),
		ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Completed, Background: 1}),
	)
	// The agent starts a turn by itself after the background task.
	feed(t, m, id,
		ag(agent.Event{Kind: agent.TurnStarted, StartedBy: agent.ByAgent}),
		ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Completed}),
	)
	// A turn that blocks on a question.
	feed(t, m, id,
		Input{Kind: Send, Principal: "hook-1", Key: "m2", Text: "ask me"},
		ag(agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}}),
	)
	// A turn that fails.
	feed(t, m, id,
		Input{Kind: Answer, Principal: "hook-1", Key: "m3", Text: "this"},
		ag(agent.Event{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.Limit}),
	)

	got := ended(t, m)
	want := []map[string]any{
		{"by": "caller", "principal": "hook-1", "outcome": "completion", "background": 1.0},
		{"by": "agent", "outcome": "completion", "background": 0.0},
		{"by": "caller", "principal": "hook-1", "outcome": "blocked", "background": 0.0},
		{"by": "caller", "principal": "hook-1", "outcome": "failure", "reason": "limit", "background": 0.0},
	}
	if len(got) != len(want) {
		t.Fatalf("%d end events for %d turns: %v", len(got), len(want), got)
	}
	started := 0
	for _, e := range events(t, m) {
		if e.Kind == KindTurnStarted {
			started++
		}
	}
	if started != len(want) {
		t.Fatalf("%d turns started, %d ended", started, len(want))
	}
	for i, w := range want {
		for k, v := range w {
			if got[i][k] != v {
				t.Errorf("end event %d: %s = %v, want %v", i, k, got[i][k], v)
			}
		}
		if _, ok := got[i]["reason"]; ok != (w["reason"] != nil) {
			t.Errorf("end event %d: reason = %v", i, got[i]["reason"])
		}
	}
}

func TestExitDuringTurnIsCrashed(t *testing.T) {
	m, id := newMachine(t, control.Idle)
	feed(t, m, id, Input{Kind: Send, Key: "m1"}, Input{Kind: Exit})
	got := ended(t, m)
	if len(got) != 1 || got[0]["outcome"] != "failure" || got[0]["reason"] != "crashed" {
		t.Fatalf("end events: %v", got)
	}
	var st string
	m.Store.DB.QueryRow(`SELECT status || '/' || reason FROM turns`).Scan(&st)
	if st != "failure/crashed" {
		t.Fatalf("turn record: %s", st)
	}
}

func TestFailureBeforeCommitLeavesNothing(t *testing.T) {
	m, id := newMachine(t, control.Idle)
	m.beforeCommit = func() error { return errors.New("injected") }
	if err := m.Apply(context.Background(), id, Input{Kind: Send, Key: "m1"}); err == nil {
		t.Fatal("Apply succeeded")
	}
	var state string
	var turns int
	m.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state)
	m.Store.DB.QueryRow(`SELECT count(*) FROM turns`).Scan(&turns)
	if state != "idle" || turns != 0 || len(events(t, m)) != 0 {
		t.Fatalf("after a failed commit: state %s, %d turns, %d events", state, turns, len(events(t, m)))
	}
}

func TestRefusedInputWritesNothing(t *testing.T) {
	m, id := newMachine(t, control.Asleep)
	if err := m.Apply(context.Background(), id, Input{Kind: Send, Key: "m1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("Send to an asleep session: %v", err)
	}
	if n := len(events(t, m)); n != 0 {
		t.Fatalf("%d events written", n)
	}
}

func TestSameAgentEventTwice(t *testing.T) {
	m, id := newMachine(t, control.Idle)
	feed(t, m, id, Input{Kind: Send, Key: "m1"})
	text := ag(agent.Event{Kind: agent.Text, Text: "once"})
	end := ag(agent.Event{Kind: agent.TurnEnded})
	feed(t, m, id, text, end, text, end) // the same (session, sequence) again
	n := 0
	for _, e := range events(t, m) {
		if e.Kind == "text" || e.Kind == KindTurnEnded {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d text and end events, want 2", n)
	}
}
