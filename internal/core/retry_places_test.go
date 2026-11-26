package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

func retryChoice(t *testing.T, k *talks, id control.ThreadID, sid int64, choice string) {
	t.Helper()
	a := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpTrust})
	var err error
	if choice == "trust" {
		err = a.Trust(context.Background(), id, sid, []string{"settings.json"})
	} else {
		err = a.StartWithoutSettings(context.Background(), id, sid)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A start-up dialog released its conversation's place. Answering it must
// acquire another place in order, with the original launch intact (#929).
func TestConversationRetryWaitsForAPlace(t *testing.T) {
	for _, choice := range []string{"trust", "without settings"} {
		for _, occupied := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/occupied=%v", choice, occupied), func(t *testing.T) {
				k := newTalks(t, 1)
				r := &earlierRunner{fakeRunner: k.runner}
				k.core.Runner = r
				id := k.call(t, "retry")
				sid := k.session(t, id)
				var fi string
				if err := k.core.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = ?`, sid).Scan(&fi); err != nil {
					t.Fatal(err)
				}
				var first FirstInput
				if err := json.Unmarshal([]byte(fi), &first); err != nil {
					t.Fatal(err)
				}
				first.Context, first.Before, first.After, first.Via = "earlier context", "before", "after", "chat"
				data, _ := json.Marshal(first)
				if _, err := k.core.Store.DB.Exec(`UPDATE sessions SET first_input = ? WHERE id = ?`, string(data), sid); err != nil {
					t.Fatal(err)
				}
				k.event(t, id, agent.Event{Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Trust: []string{"settings.json"}})
				busy := k.runs(t, "busy")
				older := k.call(t, "older")
				if !occupied {
					k.ends(t, busy) // a free place still belongs to the older waiter
				}
				want(t, "retry while another waits", k.got(t, func() { retryChoice(t, k, id, sid, choice) }), "")
				if k.session(t, id) == sid || k.threadState(t, id) != control.Asleep {
					t.Fatal("retry needs a new, sleeping session until it gets a place")
				}
				if n := k.waits(t); n != 2 {
					t.Fatalf("%d waiting inputs, want older and retry", n)
				}
				want(t, "recovery while waiting", k.got(t, func() {
					if err := k.core.Recover(context.Background()); err != nil {
						t.Fatal(err)
					}
				}), "")
				if occupied {
					want(t, "full pass", k.pass(t), "")
					want(t, "retry's waiting line", k.lines(t, id), fmt.Sprintf("[%d]", busy))
					want(t, "another full pass", k.pass(t), "")
					want(t, "line once", k.lines(t, id), fmt.Sprintf("[%d]", busy))
					k.ends(t, busy)
				}
				want(t, "older goes first", k.pass(t), "start older")
				if r.launches[len(r.launches)-1].Thread != older {
					t.Fatal("retry overtook older input")
				}
				k.ready(t, older)
				k.ends(t, older)
				want(t, "retry gets its place", k.pass(t), "start retry")
				got := r.launches[len(r.launches)-1]
				if got.Thread != id || got.Session == sid || got.Dir != first.Dir || got.Key != first.Key ||
					got.Principal != first.Principal || got.From != first.From || got.Context != first.Context || !reflect.DeepEqual(got.Input, first.Input()) || !got.Confined {
					t.Fatalf("retry launch %+v, want original input %+v", got, first)
				}
				want(t, "retry is handed over once", k.pass(t), "")
				if n := k.waits(t); n != 0 {
					t.Fatalf("%d waiting inputs after retry", n)
				}
			})
		}
	}
}

// Other thread kinds and a conversation with room still restart at once.
func TestRetryWithoutAConversationWait(t *testing.T) {
	for _, choice := range []string{"trust", "without settings"} {
		for _, kind := range []control.ThreadKind{control.KindConversation, control.KindSupervisor, control.KindLookup, control.KindOrdinary} {
			t.Run(choice+"/"+string(kind), func(t *testing.T) {
				k := newTalks(t, 1)
				var id control.ThreadID
				if kind == control.KindOrdinary {
					var err error
					id, err = k.owner.Start(context.Background(), control.StartRequest{Key: "retry", Scope: "s", Thread: "retry", Input: control.Input{Text: "retry"}})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					id = k.kind(t, kind, "retry")
				}
				sid := k.session(t, id)
				k.event(t, id, agent.Event{Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Trust: []string{"settings.json"}})
				if kind != control.KindConversation {
					k.runs(t, "busy")
					k.call(t, "older")
				}
				want(t, "retry starts at once", k.got(t, func() { retryChoice(t, k, id, sid, choice) }), "start retry")
				if k.threadState(t, id) != control.Creating || k.session(t, id) == sid || k.lines(t, id) != "" {
					t.Fatal("an immediate retry must create a session without a waiting line")
				}
			})
		}
	}
}
