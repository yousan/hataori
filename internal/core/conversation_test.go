package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

type cannotCheckLinks struct{ control.ScopeOwner }

func TestConfinedLinksRequireClientPrivacyCheck(t *testing.T) {
	k := newKinds(t)
	id := k.start(t, control.KindConversation, "public")
	k.core.Owners["chat"] = cannotCheckLinks{fakeOwner{key: "opened"}}
	ctx := context.Background()
	before := len(links(t, k.fixture, id))
	if _, err := k.owner.Link(ctx, id, control.LinkRef{Client: "chat", Channel: "s", Thread: "unknown"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Link: %v", err)
	}
	if got := len(links(t, k.fixture, id)); got != before {
		t.Fatalf("links: %d, want %d", got, before)
	}
	if got, err := k.try(control.KindConversation, "unknown-start"); !errors.Is(err, control.ErrForbidden) || got != 0 {
		t.Fatalf("start: %d, %v", got, err)
	}
	// A conversation the client opens itself remains supported even when
	// the client cannot inspect the privacy of existing conversations.
	if got, err := k.core.StartConfined(ctx, control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all},
		control.StartRequest{Scope: "s", Key: "opened", Input: control.Input{Text: "hello"}}, control.KindConversation, InDir(k.dir)); err != nil || got == 0 {
		t.Fatalf("opened: %d, %v", got, err)
	}
}

func TestConversationRetryKeepsFailedResumeContext(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprint("queued=", queued), func(t *testing.T) {
			k := newKinds(t)
			er := &earlierRunner{fakeRunner: k.runner}
			k.core.Runner = er
			id := k.start(t, control.KindConversation, "resuming")
			ctx := context.Background()
			sid, _, err := k.core.current(ctx, k.core.Store.DB, id)
			if err != nil {
				t.Fatal(err)
			}
			// As a wake records it before the runner attempts to resume the agent.
			if _, err := k.core.Store.DB.Exec(`UPDATE sessions SET state = 'creating', first_input = json_set(first_input, '$.wake', json('true')) WHERE id = ?`, sid); err != nil {
				t.Fatal(err)
			}
			if _, err := k.core.Store.DB.Exec(`UPDATE threads SET state = 'creating' WHERE id = ?`, int64(id)); err != nil {
				t.Fatal(err)
			}
			if err := k.core.Machine.Apply(ctx, sid, session.Input{Kind: session.Exit, Reason: "resume: conversation unavailable"}); err != nil {
				t.Fatal(err)
			}
			var blocker control.ThreadID
			if queued {
				sc := k.core.Scopes["s"]
				sc.ConversationsAtOnce = 1
				k.core.Scopes["s"] = sc
				blocker = k.start(t, control.KindConversation, "blocker")
				if err := k.owner.Send(ctx, blocker, "busy", control.Input{Text: "busy"}, control.Deliver); err != nil {
					t.Fatal(err)
				}
			}
			before := len(er.launches)
			if err := k.core.RetryConversation(ctx, control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all}, id, "next-call", control.Input{Text: "continue"}); err != nil {
				t.Fatal(err)
			}
			if queued {
				if len(er.launches) != before {
					t.Fatal("retry did not wait for a place")
				}
				bs, _, err := k.core.current(ctx, k.core.Store.DB, blocker)
				if err != nil {
					t.Fatal(err)
				}
				k.agentEvent(t, bs, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
				next, hand, err := k.core.giveNext(ctx, "s", nil)
				if err != nil || next != id || hand == nil {
					t.Fatalf("waiting retry: %d, %v", next, err)
				}
				if err := hand(); err != nil {
					t.Fatal(err)
				}
			}
			last := er.launches[len(er.launches)-1]
			if last.Context == "" || last.Input.Text != "continue" {
				t.Fatalf("retry: %+v", last)
			}
			var raw string
			if err := k.core.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = ?`, last.Session).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var first FirstInput
			if err := json.Unmarshal([]byte(raw), &first); err != nil {
				t.Fatal(err)
			}
			if first.Context != last.Context || first.Text != "continue" {
				t.Fatalf("stored retry: %+v", first)
			}
		})
	}
}
