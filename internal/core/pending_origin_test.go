package core

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

func TestPendingInputKeepsItsCallerAndOwnTurn(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(strconv.FormatBool(held), func(t *testing.T) {
			f := newFixture(t, fakeOwner{})
			f.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:sender"}}}, "owner", nil)
			id, sid := f.running(t, "t1")
			if !held {
				f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
			}
			a := f.core.As(control.Caller{Principal: "owner", From: "chat:sender", Via: "dashboard", Scopes: []string{"s"}, May: all})
			if err := a.Send(context.Background(), id, "held-key", control.Input{Text: "original"}, control.Deliver); err != nil {
				t.Fatal(err)
			}
			var data string
			if err := f.core.Store.DB.QueryRow(`SELECT data FROM pending_inputs WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sid).Scan(&data); err != nil {
				t.Fatal(err)
			}
			var p session.PendingInput
			if err := json.Unmarshal([]byte(data), &p); err != nil {
				t.Fatal(err)
			}
			if p.Origin == nil {
				t.Fatal("pending input has no caller")
			}
			o := p.Origin
			if o.Key != "held-key" || o.From != "chat:sender" || o.Principal != "owner" || o.Text != "original" || o.Via != "dashboard" || (o.Turn == 0) != held {
				t.Fatalf("origin %+v, held %v", o, held)
			}
		})
	}
}
