package runner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/store"
)

type rejectsContext struct{ agent.Adapter }
type rejectsContextCodec struct{ agent.Codec }

func (a rejectsContext) NewCodec(s agent.Spec) agent.Codec {
	return rejectsContextCodec{a.Adapter.NewCodec(s)}
}
func (c rejectsContextCodec) Input(in agent.Input) (agent.Actions, error) {
	if strings.HasSuffix(in.Text, "reply missed") {
		return agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Input: in.ID, Status: agent.Failed, Reason: agent.NotDelivered}}}, nil
	}
	return c.Codec.Input(in)
}

func TestTurnContextSurvivesUndeliveredInput(t *testing.T) {
	rec := &recorder{inputs: map[string][]string{}}
	f := newFixture(t, options{wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rejectsContext{rec} }})
	f.start("reply hello")
	waitFor(t, "first turn", 15*time.Second, f.has("turn_ended", 1))
	first := true
	f.core.TurnContext = func(context.Context, *store.Tx, int64) (string, error) {
		if !first {
			return "", nil
		}
		first = false
		return "Changed: notes/plan.md\n\n", nil
	}
	send(t, owner(f.core), thread, "missed", "reply missed")
	waitFor(t, "failed input", 15*time.Second, f.has("turn_ended", 2))
	send(t, owner(f.core), thread, "after", "reply after")
	waitFor(t, "delivered input", 15*time.Second, func() bool { return withText(t, f.st, "text", "after") == 1 })
	send(t, owner(f.core), thread, "last", "reply last")
	waitFor(t, "last input", 15*time.Second, func() bool { return withText(t, f.st, "text", "last") == 1 })
	want := []string{"reply hello", "Changed: notes/plan.md\n\nreply after", "reply last"}
	if got := rec.of(f.root, 1); !slices.Equal(got, want) {
		t.Fatalf("inputs %q, want %q", got, want)
	}
}

func TestReplacementGetsTurnContext(t *testing.T) {
	conv := t.TempDir()
	rec := &recorder{inputs: map[string][]string{}}
	f := newFixture(t, options{conversations: conv, wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.run.TurnContext = func(_ context.Context, _ *store.Tx, sid int64) (string, error) {
		if sid == 2 {
			return "Changed: notes/plan.md\n\n", nil
		}
		return "", nil
	}
	f.start("reply hello")
	waitFor(t, "first turn", 15*time.Second, f.has("turn_ended", 1))
	f.sleepNow(t, 1)
	files, _ := filepath.Glob(filepath.Join(conv, "*"))
	for _, p := range files {
		removeConversation(t, p)
	}
	send(t, owner(f.core), thread, "replacement", "reply after")
	waitFor(t, "replacement turn", 15*time.Second, func() bool { return withText(t, f.st, "text", "after") == 1 })
	got := rec.of(f.root, 2)
	if len(got) != 1 || !strings.Contains(got[0], "Changed: notes/plan.md\n\nreply after") {
		t.Fatalf("replacement inputs: %q", got)
	}
}

// A successful queued input consumes only its already owed prefix; context
// unique to another input that failed stays owed, without repeating success.
func TestQueuedTurnContextsSettleIndependently(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	waitFor(t, "first turn", 15*time.Second, f.has("turn_ended", 1))
	ctx := context.Background()
	err := f.st.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context='A' WHERE id=1`); err != nil {
			return err
		}
		for _, q := range []string{
			`INSERT INTO pending_inputs(id,session_id,data,context) VALUES(42,1,'{"text":"x","new_context":"B"}','AB')`,
			`INSERT INTO pending_inputs(id,session_id,data,context) VALUES(43,1,'{"text":"y","new_context":"C"}','C')`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		if err := clearReported(ctx, tx, 1, []agent.Event{{Kind: agent.TurnEnded, Input: "43", Reason: agent.NotDelivered, Status: agent.Failed}}); err != nil {
			return err
		}
		if err := preserveContext(ctx, tx, 1, "A"); err != nil {
			return err
		} // failed first input
		return clearReported(ctx, tx, 1, []agent.Event{{Kind: agent.InputTaken, Input: "42"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var owed string
	if err := f.st.DB.QueryRow(`SELECT context FROM sessions WHERE id=1`).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if owed != "C" {
		t.Fatalf("owed %q, want only failed C", owed)
	}
}

func TestNewAgentPreservesNewPendingContexts(t *testing.T) {
	for _, written := range []string{"0", "1"} {
		t.Run("written="+written, func(t *testing.T) {
			f := newFixture(t, options{})
			f.start("reply hello")
			waitFor(t, "first turn", 15*time.Second, f.has("turn_ended", 1))
			if _, err := f.st.DB.Exec(`UPDATE sessions SET context='A' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs(id,session_id,data,context,written) VALUES(42,1,'{"text":"x","new_context":"B"}','AB',` + written + `)`); err != nil {
				t.Fatal(err)
			}
			first := firstInput{Context: "A"}
			if err := f.run.newAgent(context.Background(), 1, &first); err != nil {
				t.Fatal(err)
			}
			var owed string
			if err := f.st.DB.QueryRow(`SELECT context FROM sessions WHERE id=1`).Scan(&owed); err != nil {
				t.Fatal(err)
			}
			if owed != "AB" || first.Context != "AB" {
				t.Fatalf("owed %q, launch %q", owed, first.Context)
			}
		})
	}
}

// A codec error or a missing live agent drops the pending call through
// dropPending, rather than reporting a NotDelivered event.
func TestDroppedInputPreservesNewTurnContext(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	waitFor(t, "first turn", 15*time.Second, f.has("turn_ended", 1))
	if _, err := f.st.DB.Exec(`UPDATE sessions SET context='A' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs(id,session_id,data,context) VALUES(42,1,'{"text":"x","new_context":"B"}','AB')`); err != nil {
		t.Fatal(err)
	}
	f.run.dropPending(42)
	f.run.dropPending(42) // repeated cleanup must not add the notice twice
	var owed string
	if err := f.st.DB.QueryRow(`SELECT context FROM sessions WHERE id=1`).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if owed != "AB" {
		t.Fatalf("owed %q, want AB", owed)
	}
}
