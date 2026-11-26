package core

import (
	"context"
	"errors"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

func TestWorkerHomeFollowsAcceptedConfinedInput(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	talk := k.start(t, control.KindConversation, "talk")
	own := k.start(t, control.KindSupervisor, "own")
	a := k.as(talk, false)
	w, err := a.Start(ctx, control.StartRequest{Scope: "s", Key: "worker", Thread: "worker", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	sid := k.session(t, w)
	k.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
	home := func(want control.ThreadID, count int) {
		t.Helper()
		var got control.ThreadID
		var n int
		if err := k.core.Store.DB.QueryRow(`SELECT home FROM thread_homes WHERE thread = ? ORDER BY after DESC LIMIT 1`, w).Scan(&got); err != nil {
			t.Fatal(err)
		}
		k.core.Store.DB.QueryRow(`SELECT count(*) FROM thread_homes WHERE thread = ?`, w).Scan(&n)
		if got != want || n != count {
			t.Fatalf("home %d, %d assignments; want %d, %d", got, n, want, count)
		}
	}
	home(talk, 1)
	person := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	if err := person.Send(ctx, w, "person", control.Input{Text: "hello"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	home(talk, 1)
	k.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	if err := k.as(own, false).Send(ctx, w, "own-send", control.Input{Text: "next"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	home(own, 2)
	// Replaying a formerly accepted call must not take the worker back.
	if _, err := a.Start(ctx, control.StartRequest{Scope: "s", Key: "worker", Thread: "worker", Input: control.Input{Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	home(own, 2)

	if err := a.Send(ctx, w, "busy", control.Input{Text: "no"}, control.Refuse); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy send: %v", err)
	}
	home(own, 2)
}

func TestApprovedProposalKeepsItsConfinedOrigin(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.Owners["chat"] = fakeOwner{key: "proposed-worker"}
	talk := k.start(t, control.KindConversation, "talk")
	k.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	proposer := k.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: all, AsksIn: talk, Propose: true, OrdinaryOnly: true})
	_, err := proposer.Start(ctx, control.StartRequest{Scope: "s", Key: "proposed", Thread: "proposed", Input: control.Input{Text: "go"}})
	var p *control.Proposed
	if !errors.As(err, &p) {
		t.Fatalf("proposing: %v", err)
	}
	var handle string
	if err := k.core.Store.DB.QueryRow(`SELECT handle FROM proposals WHERE id = ?`, p.Number).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	person := k.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
	if err := person.Answer(ctx, control.RequestID(handle), control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	var home control.ThreadID
	if err := k.core.Store.DB.QueryRow(`SELECT home FROM thread_homes`).Scan(&home); err != nil || home != talk {
		t.Fatalf("approved start home %d: %v", home, err)
	}
}
