package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/session"
)

// A Stop that names a turn hands the runner that turn, which checks it in
// the step that interrupts; one for a thread that runs no turn is refused
// at once, and nothing is interrupted (#457).
func TestStopNamesItsTurn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, sid := f.running(t, "t1")
	var turn int64
	if err := f.core.Store.DB.QueryRow(`SELECT id FROM turns WHERE session_id = ? AND ended_at IS NULL`, sid).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.Calls()
	if err := f.owner.Stop(ctx, id, turn); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimPrefix(f.runner.Calls(), calls+"; "), fmt.Sprintf("interrupt turn %d", turn); got != want {
		t.Fatalf("runner calls %q, want %q", got, want)
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	calls = f.runner.Calls()
	if err := f.owner.Stop(ctx, id, turn); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("Stop of the ended turn of an idle thread: %v, want refused", err)
	}
	if err := f.owner.Stop(ctx, id, 0); err != nil {
		t.Fatalf("Stop of whatever runs, when nothing does: %v", err)
	}
	if got := f.runner.Calls(); got != calls {
		t.Fatalf("the runner was called: %q", strings.TrimPrefix(got, calls))
	}
}
