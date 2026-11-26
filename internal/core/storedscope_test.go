package core

import (
	"context"
	"errors"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

func TestStoredScopeOrdinaryStartsRequireHostCommand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	sc := f.core.Scopes["s"]
	sc.Unlisted = true
	f.core.Scopes["s"] = sc
	for name, caller := range map[string]control.Caller{
		"owner":     {Principal: "owner", Scopes: []string{"s"}, May: all},
		"dashboard": {Principal: "owner", Via: "dashboard", Scopes: []string{"s"}, May: all},
		"chat":      {From: "chat:owner", Scopes: []string{"s"}, May: all},
		"proposal":  {Principal: "owner", Via: control.ViaProposal, Scopes: []string{"s"}, May: all},
	} {
		before := count(t, f.core.Store, "threads")
		_, err := f.core.As(caller).Start(ctx, control.StartRequest{Scope: "s", Key: name, Input: control.Input{Text: "go"}})
		if !errors.Is(err, ErrForbidden) || count(t, f.core.Store, "threads") != before {
			t.Fatalf("%s: %v", name, err)
		}
	}
	host := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, StartUnlisted: true})
	if _, err := host.Start(ctx, control.StartRequest{Scope: "s", Key: "host", Input: control.Input{Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	policies, err := host.Scopes(ctx)
	if err != nil || len(policies) != 1 || !policies[0].Unlisted {
		t.Fatalf("%+v %v", policies, err)
	}
}

func TestStoredScopeConversationOnlyOwnerInput(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	k.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:owner"}}, {Name: "other", IDs: []string{"chat:other"}}}, "owner", map[string][]string{"elsewhere": {"other"}})
	sc := k.core.Scopes["s"]
	sc.Unlisted = true
	k.core.Scopes["s"] = sc
	id := k.start(t, control.KindConversation, "conversation")
	if _, err := k.try(control.KindSupervisor, "own"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("own thread: %v", err)
	}
	for _, from := range []string{"chat:other", "chat:guest", "supervisor:s"} {
		api := k.core.As(control.Caller{From: from, Scopes: []string{"s"}, May: all})
		if err := api.Send(ctx, id, from, control.Input{Text: "reach agent"}, control.Deliver); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v", from, err)
		}
	}
	owner := k.core.As(control.Caller{From: "chat:owner", Scopes: []string{"s"}, May: all})
	if err := owner.Send(ctx, id, "owner", control.Input{Text: "owner input"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
}

func TestStoredScopeApprovedStartIsRechecked(t *testing.T) {
	ctx := context.Background()
	f, proposer, asksIn, _ := proposalFixture(t)
	n := proposalOf(t, func() error {
		_, err := proposer.Start(ctx, control.StartRequest{Scope: "s", Key: "before-unlisted", Input: control.Input{Text: "proposal start"}})
		return err
	}())
	d, err := f.owner.Thread(ctx, asksIn, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	sc := f.core.Scopes["s"]
	sc.Unlisted = true
	f.core.Scopes["s"] = sc
	before := count(t, f.core.Store, "threads")
	// Retain proposer authorization to isolate the start-policy recheck.
	// Approval records its failed outcome rather than returning the call's
	// error; the important boundary is that no ordinary thread is started.
	if err := f.owner.Answer(ctx, d.Requests[0].ID, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if count(t, f.core.Store, "threads") != before {
		t.Fatalf("proposal %d started a thread", n)
	}
	var outcome string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM proposals WHERE id=?`, n).Scan(&outcome); err != nil || outcome != "failed" {
		t.Fatalf("%q %v", outcome, err)
	}
}
