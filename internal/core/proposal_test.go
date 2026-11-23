package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// proposalFixture is a fixture whose scope s has the supervisor's person
// sv, a running thread the supervisor proposes in, and an idle target.
func proposalFixture(t *testing.T) (*fixture, control.API, control.ThreadID, control.ThreadID) {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	asksIn, _ := f.running(t, "asker")
	target, err := f.owner.Start(ctx, control.StartRequest{Key: "target", Scope: "s", Thread: "target", Input: control.Input{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := f.core.current(ctx, f.core.Store.DB, target)
	if err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
	proposer := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpSend | control.OpStart,
		AsksIn: asksIn, Mark: "supervisor", RefuseBusy: true, Propose: true})
	return f, proposer, asksIn, target
}

// proposalOf returns the number of the proposal err is.
func proposalOf(t *testing.T, err error) int64 {
	t.Helper()
	var p *control.Proposed
	if !errors.As(err, &p) || !errors.Is(err, control.ErrProposed) || p.Number <= 0 {
		t.Fatalf("not a proposal: %v", err)
	}
	return p.Number
}

// approveTwice approves req twice at once, as two presses would.
func approveTwice(t *testing.T, f *fixture, req control.RequestID) {
	t.Helper()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.owner.Answer(context.Background(), req, control.Answer{Choices: []string{"approve"}}); err != nil && !errors.Is(err, ErrNotOpen) {
				t.Errorf("answer: %v", err)
			}
		}()
	}
	wg.Wait()
}

// A proposed send gives the thread nothing until a person approves it,
// and then runs once, under the key made from the proposal: also when an
// earlier approval made the call and the daemon died before it recorded
// the outcome, and when it is approved twice at once (#650).
func TestProposedSendRunsOnceUnderItsKey(t *testing.T) {
	ctx := context.Background()
	f, proposer, asksIn, target := proposalFixture(t)
	n := proposalOf(t, proposer.Send(ctx, target, "k", control.Input{Text: "do it"}, control.Refuse))
	if again := proposalOf(t, proposer.Send(ctx, target, "k", control.Input{Text: "do it"}, control.Refuse)); again != n {
		t.Fatalf("a repeated key made proposal %d, not %d", again, n)
	}
	if calls := f.runner.Calls(); strings.Contains(calls, "do it") {
		t.Fatalf("the proposal reached the runner: %s", calls)
	}
	d, err := f.owner.Thread(ctx, asksIn, 0, 0)
	if err != nil || len(d.Requests) != 1 || d.Requests[0].Kind != control.Approval {
		t.Fatalf("requests: %+v %v", d.Requests, err)
	}
	req := d.Requests[0].ID
	self := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAnswer})
	if err := self.Answer(ctx, req, control.Answer{Allow: true}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("the proposer's own approval: %v", err)
	}
	// An earlier approval made the call, and its daemon died before it
	// recorded the outcome.
	earlier := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpSend, RefuseBusy: true, NoAnswer: true,
		Mark: "supervisor", Via: control.ViaProposal})
	if err := earlier.Send(ctx, target, "proposal:"+string(req), control.Input{Text: "do it"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	approveTwice(t, f, req)
	if calls := f.runner.Calls(); strings.Count(calls, "supervisor\ndo it") != 1 {
		t.Fatalf("runner calls: %s", calls)
	}
	ev := eventsOf(t, f, asksIn, "proposal_answered")
	if len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"run"`) {
		t.Fatalf("proposal_answered: %v", ev)
	}
}

// A proposed start makes no thread until a person approves it, and then
// one thread, once, with the model and the effort it showed.
func TestProposedStartRunsOnceUnderItsKey(t *testing.T) {
	ctx := context.Background()
	f, proposer, asksIn, _ := proposalFixture(t)
	threads := count(t, f.core.Store, "threads")
	_, err := proposer.Start(ctx, control.StartRequest{Key: "k", Scope: "s", Repo: "repo", Input: control.Input{Text: "build it"}, Effort: "low"})
	proposalOf(t, err)
	if got := count(t, f.core.Store, "threads"); got != threads {
		t.Fatalf("%d threads after the proposal, want %d", got, threads)
	}
	made := eventsOf(t, f, asksIn, "proposal_made")
	if len(made) != 1 || !strings.Contains(made[0], `"effort":"low"`) || !strings.Contains(made[0], `"action":"start"`) {
		t.Fatalf("proposal_made: %v", made)
	}
	d, err := f.owner.Thread(ctx, asksIn, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("requests: %+v %v", d.Requests, err)
	}
	req := d.Requests[0].ID
	// An earlier approval made the call, and its daemon died before it
	// recorded the outcome.
	earlier := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpStart, Mark: "supervisor", Via: control.ViaProposal})
	if _, err := earlier.Start(ctx, control.StartRequest{Key: "proposal:" + string(req), Scope: "s", Repo: "repo",
		Input: control.Input{Text: "build it"}, Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	approveTwice(t, f, req)
	if got := count(t, f.core.Store, "threads"); got != threads+1 {
		t.Fatalf("%d threads after the approvals, want %d", got, threads+1)
	}
	if calls := f.runner.Calls(); strings.Count(calls, "start supervisor\nbuild it") != 1 {
		t.Fatalf("runner calls: %s", calls)
	}
	var effort string
	f.core.Store.DB.QueryRow(`SELECT effort FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&effort)
	if effort != "low" {
		t.Fatalf("the started thread's effort: %q", effort)
	}
}

// eventsOf returns the data of thread id's events of kind.
func eventsOf(t *testing.T, f *fixture, id control.ThreadID, kind string) []string {
	t.Helper()
	rows, err := f.core.Store.DB.Query(`SELECT data FROM events WHERE thread_id = ? AND kind = ? ORDER BY pos`, int64(id), kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

// An approval whose call took the input and failed after it, before the
// outcome was stored, leaves the proposal approved and listed. The next
// answer, a refusal too, gives the thread nothing more and records the
// outcome as the first approval's: the send ran, also when the thread was
// closed since (#650).
func TestProposalApprovedBeforeItsOutcome(t *testing.T) {
	ctx := context.Background()
	f, proposer, asksIn, target := proposalFixture(t)
	proposalOf(t, proposer.Send(ctx, target, "k", control.Input{Text: "do it"}, control.Refuse))
	d, err := f.owner.Thread(ctx, asksIn, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("requests: %+v %v", d.Requests, err)
	}
	req := d.Requests[0].ID
	f.runner.mu.Lock()
	f.runner.deliverErr = errors.New("the session is not reconnected yet")
	f.runner.mu.Unlock()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err == nil {
		t.Fatal("the approval whose delivery failed succeeded")
	}
	if d, err := f.owner.Thread(ctx, asksIn, 0, 0); err != nil || len(d.Requests) != 1 || d.Requests[0].ID != req {
		t.Fatalf("after the failed delivery, requests: %+v %v", d.Requests, err)
	}
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}, {Name: "other", IDs: []string{"chat:other"}}},
		"owner", map[string][]string{"s": {"sv", "other"}})
	// Meanwhile the runner handed the input over, the turn ran, and a person
	// closed the thread.
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET state = 'closed' WHERE thread_id = ?`, int64(target)); err != nil {
		t.Fatal(err)
	}
	other := f.core.As(control.Caller{From: "chat:other", Scopes: []string{"s"}, May: control.OpAnswer})
	if err := other.Answer(ctx, req, control.Answer{Choices: []string{"refuse"}}); err != nil {
		t.Fatal(err)
	}
	ev := eventsOf(t, f, asksIn, "proposal_answered")
	if len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"run"`) || !strings.Contains(ev[0], `"allow":true`) || !strings.Contains(ev[0], `"principal":"owner"`) {
		t.Fatalf("proposal_answered: %v", ev)
	}
	if calls := f.runner.Calls(); strings.Contains(calls, "deliver supervisor\ndo it") {
		t.Fatalf("delivered again: %s", calls)
	}
	if err := other.Answer(ctx, req, control.Answer{Allow: true}); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a third answer: %v", err)
	}
}
