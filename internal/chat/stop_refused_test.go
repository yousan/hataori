package chat

import (
	"context"
	"fmt"

	"github.com/yousan/hataori/internal/agent"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

type refusedButtonStop struct{ *starts }

func (refusedButtonStop) Interrupt(context.Context, int64, int64, session.Stopper) error {
	return fmt.Errorf("the window is in copy mode: %w", control.ErrNotInterrupted)
}

func TestARefusedStopButtonAnswersItsPresser(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	f.core.Runner = refusedButtonStop{f.runner}
	var turn uint64
	if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT pos FROM events WHERE thread_id = ? AND kind = 'turn_started' ORDER BY pos DESC LIMIT 1`, int64(id)).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	token := stopChoice(id, turn).Token
	press := surface.Selection{Ref: surface.MessageRef{Conv: f.conv(id), ID: "line-1"}, Author: fake.Person(owner), Token: token, Tokens: []string{token}}
	f.fe.Choice(press)
	f.fe.Choice(press)
	calls := f.postedUntil(t, id, "Nothing was stopped: the window is in copy mode: the agent's window did not take the interrupt; the turn goes on.")
	m := calls[len(calls)-1].Message
	if m.ReplyTo == nil || *m.ReplyTo != press.Ref || len(m.Notify) != 1 || m.Notify[0] != owner {
		t.Fatalf("refusal did not answer the presser: %+v", m)
	}
	if stopped, err := f.fe.stopped(context.Background(), id, turn); err != nil || stopped {
		t.Fatalf("refused Stop recorded as accepted: %v, %v", stopped, err)
	}
}

// A delayed handoff remains in a running turn, and replaying its durable
// event after reconnect does not post the notice again.
func TestAnOwedEnterIsShownInTheThreadOnce(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	f.showing(t)
	in := session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 2, Kind: agent.InputWaiting, Input: "waiting", Reason: "Enter could not be handed to the agent's window"}}
	for range 2 {
		if err := f.core.Machine.Apply(context.Background(), 1, in); err != nil {
			t.Fatal(err)
		}
	}
	text := "The message is waiting: Enter could not be handed to the agent's window."
	calls := f.postedUntil(t, id, text)
	n := 0
	for _, call := range calls {
		if call.Message.Text == text {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d waiting notices, want one", n)
	}
	var state string
	if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id=1`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("waiting state %q, %v", state, err)
	}
}
