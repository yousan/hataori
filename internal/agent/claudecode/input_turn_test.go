package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Run the codec's captured hooks through the actual session machine: a
// failure of B must not be mistaken for the end of the turn C opened.
func TestWaitingInputsStopWithTheTurnTheyPrecede(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(strconv.FormatBool(queued), func(t *testing.T) {
			c, m, apply, send := inputTurnMachine(t)
			w := &handing{refuse: true}
			apply(decodeFile(t, c, "A", "plain/hooks/02-UserPromptSubmit.json"))
			w.act(send(41, "B"))
			if queued {
				c.ready = false // no paste is attempted while the CLI is unavailable
			}
			apply(w.act(decodeFile(t, c, "end-A", "plain/hooks/03-Stop.json")))
			w.act(send(42, "C"))
			apply(c.Interrupt())
			w.refuse, c.ready = false, true
			for now, end := time.Now(), time.Now().Add(2*DefaultInputTimeout); now.Before(end); now = now.Add(time.Second) {
				apply(w.act(c.Tick(now)))
			}
			w.want(t)
			var state, reason string
			if err := m.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&state); err != nil || state != "idle" {
				t.Fatalf("after Stop: state %q: %v", state, err)
			}
			if err := m.Store.DB.QueryRow(`SELECT reason FROM turns WHERE trigger = '42'`).Scan(&reason); err != nil || reason != session.ReasonInterrupted {
				t.Fatalf("C ended %q: %v", reason, err)
			}
		})
	}
}

func TestWaitingInputTimeoutLeavesTheNewTurnOpen(t *testing.T) {
	c, m, apply, send := inputTurnMachine(t)
	w := &handing{refuse: true}
	apply(decodeFile(t, c, "A", "plain/hooks/02-UserPromptSubmit.json"))
	w.act(send(41, "B"))
	apply(w.act(decodeFile(t, c, "end-A", "plain/hooks/03-Stop.json")))
	text := prompt(t, "plain/hooks/02-UserPromptSubmit.json")
	w.act(send(42, text))
	now := time.Now()
	apply(w.act(c.Tick(now)))
	w.refuse = false
	apply(w.act(c.Tick(now.Add(DefaultInputTimeout))))
	var state string
	if err := m.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("after B timeout: state %q: %v", state, err)
	}
	apply(decodeFile(t, c, "C", "plain/hooks/02-UserPromptSubmit.json"))
	apply(decodeFile(t, c, "end-C", "plain/hooks/03-Stop.json"))
	var status, reason string
	if err := m.Store.DB.QueryRow(`SELECT status, reason FROM turns WHERE trigger = '42'`).Scan(&status, &reason); err != nil || status != session.Completion || reason != "" {
		t.Fatalf("C ended %q/%q: %v", status, reason, err)
	}
}

func inputTurnMachine(t *testing.T) (*codec, *session.Machine, func(Actions), func(int64, string) Actions) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 'test', 'test', 'caller', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'fake', 'idle', 0, 0)`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	m, c := &session.Machine{Store: s}, started(t)
	apply := func(a Actions) {
		t.Helper()
		for _, e := range a.Events {
			if err := m.Apply(ctx, 1, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
				t.Fatalf("event %+v: %v", e, err)
			}
		}
	}
	send := func(id int64, text string) Actions {
		t.Helper()
		key := strconv.FormatInt(id, 10)
		var before string
		if err := s.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := m.Apply(ctx, 1, session.Input{Kind: session.Send, Key: key, Text: text}); err != nil {
			t.Fatal(err)
		}
		origin := &session.InputOrigin{Key: key, Text: text}
		if before == "idle" {
			if err := s.DB.QueryRow(`SELECT id FROM turns WHERE ended_at IS NULL`).Scan(&origin.Turn); err != nil {
				t.Fatal(err)
			}
		}
		b, err := json.Marshal(session.PendingInput{Input: control.Input{Text: text}, Origin: origin})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data) VALUES (?, 1, ?)`, id, string(b)); err != nil {
			t.Fatal(err)
		}
		a, err := c.Input(agent.Input{ID: key, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	return c, m, apply, send
}
