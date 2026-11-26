package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/store"
)

// lastTurnReason is the reason of the last turn_ended event.
func lastTurnReason(t *testing.T, st *store.Store) string {
	t.Helper()
	var r *string
	st.DB.QueryRow(`SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'turn_ended' ORDER BY pos DESC LIMIT 1`).Scan(&r)
	if r == nil {
		return ""
	}
	return *r
}

// The daemon dies right after it wrote input to the agent, before the
// agent's report of it is read: the new daemon tells the codec, so the
// input runs once, its id comes back and its row clears.
func TestWrittenInputIsNotWrittenAgain(t *testing.T) {
	cases := []struct {
		name  string
		setup string // the first turn's script
		ready string // the event the first turn must have reached
	}{
		{name: "send to an idle thread", setup: "reply one", ready: "turn_ended"},
		// Held by the codec during the turn, written when it ends.
		{name: "send during a turn", setup: "tool wait 2s\nreply one", ready: "tool_started"},
	}
	forms(t, func(t *testing.T, terminal bool) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				t.Cleanup(func() { killSessions(root) })
				d := runDaemon(t, root, terminal, "start+slow", tc.setup)
				st := openStore(t, root)
				until(t, "the first turn", st, tc.ready, 1)
				d.kill()
				runDaemon(t, root, terminal, "die-send+slow", "input written", "reply two").waitKilled(t)
				d = runDaemon(t, root, terminal, "resume+slow")
				until(t, "the second turn's end", st, "turn_ended", 2)
				time.Sleep(2 * time.Second) // a second copy would have been taken by now
				d.kill()
				got := counts(t, st)
				if got["turn_started"] != 2 || got["turn_ended"] != 2 || withText(t, st, "text", "two") != 1 || withText(t, st, "input", "reply two") != 1 {
					t.Errorf("events %v; %d answers \"two\"", got, withText(t, st, "text", "two"))
				}
				if r := lastTurnReason(t, st); r != "" {
					t.Errorf("the second turn ended with %q", r)
				}
				if n := count(t, st, "pending_inputs"); n != 0 {
					t.Errorf("%d pending inputs left: its id did not come back", n)
				}
			})
		}
	})
}

// The daemon dies after it marked input written and before it wrote it:
// the input is not lost silently, its turn fails as "not delivered", and
// its row clears.
func TestInputMarkedButNotWrittenFailsVisibly(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		root := t.TempDir()
		t.Cleanup(func() { killSessions(root) })
		d := runDaemon(t, root, terminal, "start", "reply one")
		st := openStore(t, root)
		until(t, "the first turn", st, "turn_ended", 1)
		d.kill()
		runDaemon(t, root, terminal, "die-send", "input marked", "reply two").waitKilled(t)
		d = runDaemon(t, root, terminal, "resume+quick")
		until(t, "the second turn's end", st, "turn_ended", 2)
		time.Sleep(300 * time.Millisecond)
		d.kill()
		if r := lastTurnReason(t, st); r != "not delivered" {
			t.Errorf("the second turn ended with %q, want \"not delivered\"", r)
		}
		if n := withText(t, st, "text", "two"); n != 0 {
			t.Errorf("%d answers \"two\"", n)
		}
		if n := count(t, st, "pending_inputs"); n != 0 {
			t.Errorf("%d pending inputs left", n)
		}
	})
}

// handed is a codec that records the input it is handed.
type handed struct {
	agent.Codec
	got map[string]agent.Input
}

func (c *handed) Input(in agent.Input) (agent.Actions, error) {
	c.got[in.ID] = in
	return agent.Actions{}, nil
}

// After a restart, the codec is told which written input a runner from
// before agent.Input.Earlier wrote to the agent that runs, and which one
// this runner wrote: the first input and a pending one alike. One written to
// an agent before it (stale) is not earlier (#914).
func TestWrittenInputSaysWhichRunnerWroteIt(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	first, _ := json.Marshal(firstInput{Text: "one", Sent: true, Written: true})
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'owner', 'running', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at, first_input) VALUES (1, 1, 'test', 'running', 0, 0, '` + string(first) + `')`,
		`INSERT INTO pending_inputs (id, session_id, request, data, written) VALUES (10, 1, '', '{"text":"two"}', 1)`,
		`INSERT INTO pending_inputs (id, session_id, request, data, written) VALUES (11, 1, '', '{"text":"three"}', 0)`,
		`INSERT INTO pending_inputs (id, session_id, request, data, written, stale) VALUES (12, 1, '', '{"text":"four"}', 1, 1)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := &handed{got: map[string]agent.Input{}}
	lv := &live{id: 1, codec: c}
	check := func(when string, want map[string]bool) {
		t.Helper()
		r.handOverWritten(lv)
		if len(c.got) != len(want) {
			t.Errorf("%s: handed over %v, want %v", when, c.got, want)
		}
		for id, earlier := range want {
			if in, ok := c.got[id]; !ok || !in.Written || in.Earlier != earlier {
				t.Errorf("%s: input %s: %+v, want it written, earlier %v", when, id, in, earlier)
			}
		}
		c.got = map[string]agent.Input{}
	}
	check("before", map[string]bool{firstInputID: true, "10": true, "12": false})
	if err := r.markWritten(lv, []string{firstInputID, "11"}); err != nil {
		t.Fatal(err)
	}
	check("after this runner wrote two", map[string]bool{firstInputID: false, "10": true, "11": false, "12": false})
	r.unmarkWritten(lv, []string{"11"})
	check("after it unmarked one", map[string]bool{firstInputID: false, "10": true, "12": false})
}
