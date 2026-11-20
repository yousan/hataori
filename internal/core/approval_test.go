package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

func TestApprovalInterruptOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	askerThread, _ := f.running(t, "asker")
	target, _ := f.running(t, "target")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: askerThread, Mark: "supervisor"})
	if _, err := asker.AskInterrupt(ctx, askerThread, "self", "urgent"); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("ask to interrupt own thread: %v", err)
	}
	req, err := asker.AskInterrupt(ctx, target, "key", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	again, err := asker.AskInterrupt(ctx, target, "different-key", "different")
	if err != nil || again != req {
		t.Fatalf("repeated ask: %q %v", again, err)
	}
	d, err := f.owner.Thread(ctx, askerThread, 0, 0)
	if err != nil || len(d.Requests) != 1 || d.Requests[0].ID != req {
		t.Fatalf("requests: %+v %v", d.Requests, err)
	}
	self := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAnswer})
	if err := self.Answer(ctx, req, control.Answer{Allow: true}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("self approval: %v", err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.owner.Answer(ctx, req, control.Answer{Choices: []string{"approve"}})
			if err != nil && !errors.Is(err, ErrNotOpen) {
				t.Errorf("answer: %v", err)
			}
		}()
	}
	wg.Wait()
	calls := f.runner.Calls()
	if strings.Count(calls, "interrupt") != 1 || strings.Count(calls, "deliver supervisor\nurgent") != 1 {
		t.Fatalf("runner calls: %s", calls)
	}
	if got := count(t, f.core.Store, "approvals"); got != 1 {
		t.Fatalf("approval rows: %d", got)
	}
}

func TestApprovalCannotInterruptLaterTurn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	askerThread, _ := f.running(t, "asker")
	target, sid := f.running(t, "target")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: askerThread})
	req, err := asker.AskInterrupt(ctx, target, "key", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnStarted})
	f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnEnded})
	if _, err := asker.AskInterrupt(ctx, target, "idle", "urgent"); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("idle ask: %v", err)
	}
	if err := f.owner.Send(ctx, target, "new-turn", control.Input{Text: "new"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	before := f.runner.Calls()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.Calls(); got != before {
		t.Fatalf("old approval reached later turn: %s", got)
	}
	var state string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM approvals WHERE handle = ?`, req).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("state %q: %v", state, err)
	}
}

func TestApprovalAlreadyInterruptedTurn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	askerThread, _ := f.running(t, "asker")
	target, _ := f.running(t, "target")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: askerThread})
	first, err := asker.AskInterrupt(ctx, target, "first", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Answer(ctx, first, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	second, err := asker.AskInterrupt(ctx, target, "second", "urgent")
	if err != nil || second == first {
		t.Fatalf("second request %q: %v", second, err)
	}
	if err := f.owner.Answer(ctx, second, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM approvals WHERE handle = ?`, second).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("state %q: %v", state, err)
	}
	if calls := f.runner.Calls(); strings.Count(calls, "interrupt") != 1 {
		t.Fatalf("runner calls: %s", calls)
	}
}

// #493: the asked turn ends, and the next one starts, after the approval
// took the reason and before the interrupt. The next turn keeps running:
// nothing is interrupted, and the outcome is ended. The reason, taken while
// the asked turn ran, is handed over once, as any input of a turn that
// ended first.
func TestApprovalOfATurnThatEndsBeforeTheInterrupt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	askerThread, _ := f.running(t, "asker")
	target, sid := f.running(t, "target")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: askerThread, Mark: "supervisor"})
	req, err := asker.AskInterrupt(ctx, target, "key", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	var asked int64
	if err := f.core.Store.DB.QueryRow(`SELECT turn_id FROM approvals WHERE handle = ?`, req).Scan(&asked); err != nil {
		t.Fatal(err)
	}
	f.runner.beforeInterrupt = func() {
		f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
		if err := f.owner.Send(ctx, target, "next", control.Input{Text: "the next turn"}, control.Deliver); err != nil {
			t.Errorf("the next turn: %v", err)
		}
	}
	before := f.runner.Calls()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if calls := strings.TrimPrefix(f.runner.Calls(), before); strings.Contains(calls, "interrupt") ||
		strings.Count(calls, "deliver supervisor\nurgent") != 1 {
		t.Fatalf("runner calls after the approval: %s", calls)
	}
	var next int64
	if err := f.core.Store.DB.QueryRow(`SELECT id FROM turns WHERE session_id = ? AND ended_at IS NULL`, sid).Scan(&next); err != nil || next == asked {
		t.Fatalf("the open turn %d (asked %d): %v", next, asked, err)
	}
	var state string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM approvals WHERE handle = ?`, req).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("approval state %q: %v", state, err)
	}
}

// #493: when the reason cannot be handed over after the interrupt was
// refused, the approval still ends as ended, and it is not left open for
// an answer that would record it as interrupted.
func TestApprovalOfATurnThatEndsBeforeTheInterruptAndADeliveryThatFails(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	askerThread, _ := f.running(t, "asker")
	target, sid := f.running(t, "target")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: askerThread})
	req, err := asker.AskInterrupt(ctx, target, "key", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	f.runner.beforeInterrupt = func() {
		f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
		if err := f.owner.Send(ctx, target, "next", control.Input{Text: "the next turn"}, control.Deliver); err != nil {
			t.Errorf("the next turn: %v", err)
		}
		f.runner.mu.Lock()
		f.runner.deliverErr = errors.New("the session is gone")
		f.runner.mu.Unlock()
	}
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatalf("the approval: %v", err)
	}
	var state string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM approvals WHERE handle = ?`, req).Scan(&state); err != nil || state != "ended" {
		t.Fatalf("approval state %q: %v", state, err)
	}
	if calls := f.runner.Calls(); strings.Contains(calls, "interrupt") {
		t.Fatalf("runner calls: %s", calls)
	}
}

// approvalFixture is a supervisor's approval asked for the running turn of
// a thread: the fixture, the target, its session, the asked turn and the
// request.
func approvalFixture(t *testing.T) (*fixture, control.ThreadID, int64, int64, control.RequestID) {
	t.Helper()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	askerThread, _ := f.running(t, "asker")
	target, sid := f.running(t, "target")
	asker := f.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAsk, AsksIn: askerThread, Mark: "supervisor"})
	req, err := asker.AskInterrupt(context.Background(), target, "key", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	var turn int64
	if err := f.core.Store.DB.QueryRow(`SELECT turn_id FROM approvals WHERE handle = ?`, req).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	return f, target, sid, turn, req
}

func approvalState(t *testing.T, f *fixture, req control.RequestID) string {
	t.Helper()
	var state string
	if err := f.core.Store.DB.QueryRow(`SELECT state FROM approvals WHERE handle = ?`, req).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// #523: the first answer's interrupt fails (the session is not reconnected
// yet), so the approval stays open with its reason taken. Answered again,
// it interrupts the asked turn, which still runs, naming it to the runner,
// and does not give the reason again.
func TestApprovalAnsweredAgainAfterItsInterruptFailed(t *testing.T) {
	ctx := context.Background()
	f, _, _, turn, req := approvalFixture(t)
	f.runner.mu.Lock()
	f.runner.interruptErr = errors.New("the session is not running")
	f.runner.mu.Unlock()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err == nil {
		t.Fatal("the first answer: no error")
	}
	if s := approvalState(t, f, req); s != "open" {
		t.Fatalf("after the first answer: %q", s)
	}
	before := f.runner.Calls()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if calls, want := strings.TrimPrefix(f.runner.Calls(), before+"; "), fmt.Sprintf("interrupt turn %d", turn); calls != want {
		t.Fatalf("runner calls of the second answer: %q, want %q", calls, want)
	}
	if s := approvalState(t, f, req); s != "interrupted" {
		t.Fatalf("after the second answer: %q", s)
	}
	if n := strings.Count(f.runner.Calls(), "deliver supervisor\nurgent"); n > 1 {
		t.Fatalf("the reason was given %d times", n)
	}
}

// #523: as above, but the asked turn ended before the approval is answered
// again: nothing is interrupted, and the outcome is ended.
func TestApprovalAnsweredAgainAfterItsInterruptFailedAndTheTurnEnded(t *testing.T) {
	ctx := context.Background()
	f, _, sid, _, req := approvalFixture(t)
	f.runner.mu.Lock()
	f.runner.interruptErr = errors.New("the session is not running")
	f.runner.mu.Unlock()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err == nil {
		t.Fatal("the first answer: no error")
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	before := f.runner.Calls()
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if calls := strings.TrimPrefix(f.runner.Calls(), before); strings.Contains(calls, "interrupt") {
		t.Fatalf("runner calls of the second answer: %q", calls)
	}
	if s := approvalState(t, f, req); s != "ended" {
		t.Fatalf("after the second answer: %q", s)
	}
}

// #523: the interrupt is refused (the asked turn ended and the next began),
// but writing the outcome fails, as when the call's context ends. Answered
// again, the approval records ended, not interrupted: the reason's key,
// taken by the first answer, does not decide the outcome.
func TestApprovalAnsweredAgainAfterItsOutcomeWasNotWritten(t *testing.T) {
	ctx := context.Background()
	f, target, sid, _, req := approvalFixture(t)
	first, cancel := context.WithCancel(ctx)
	defer cancel()
	f.runner.beforeInterrupt = func() {
		f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
		if err := f.owner.Send(ctx, target, "next", control.Input{Text: "the next turn"}, control.Deliver); err != nil {
			t.Errorf("the next turn: %v", err)
		}
		cancel()
	}
	if err := f.owner.Answer(first, req, control.Answer{Allow: true}); err == nil {
		t.Fatal("the first answer: no error")
	}
	if s := approvalState(t, f, req); s != "open" {
		t.Fatalf("after the first answer: %q", s)
	}
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if s := approvalState(t, f, req); s != "ended" {
		t.Fatalf("after the second answer: %q", s)
	}
	if calls := f.runner.Calls(); strings.Contains(calls, "interrupt") || strings.Count(calls, "deliver supervisor\nurgent") != 1 {
		t.Fatalf("runner calls: %q", calls)
	}
}

// #523: the interrupt was sent, but writing the outcome failed. Answered
// again while the asked turn still runs, the approval interrupts it again,
// naming it, and records interrupted; the reason is given once.
func TestApprovalAnsweredAgainAfterItsSentInterruptWasNotRecorded(t *testing.T) {
	ctx := context.Background()
	f, _, _, turn, req := approvalFixture(t)
	first, cancel := context.WithCancel(ctx)
	defer cancel()
	f.runner.beforeInterrupt = cancel
	if err := f.owner.Answer(first, req, control.Answer{Allow: true}); err == nil {
		t.Fatal("the first answer: no error")
	}
	if s := approvalState(t, f, req); s != "open" {
		t.Fatalf("after the first answer: %q", s)
	}
	if err := f.owner.Answer(ctx, req, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if s := approvalState(t, f, req); s != "interrupted" {
		t.Fatalf("after the second answer: %q", s)
	}
	calls := f.runner.Calls()
	if n := strings.Count(calls, fmt.Sprintf("interrupt turn %d", turn)); n != 2 || strings.Count(calls, "deliver supervisor\nurgent") != 1 {
		t.Fatalf("runner calls: %q", calls)
	}
}
