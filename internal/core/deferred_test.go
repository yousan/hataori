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
	"github.com/yousan/hataori/internal/store"
)

type deferredFixture struct {
	*fixture
	d      *DeferredInputs
	caller control.Caller
	repos  []string
	target control.ThreadID
	sid    int64
}

func newDeferredFixture(t *testing.T) *deferredFixture {
	t.Helper()
	f := newFixture(t, fakeOwner{key: "chat"})
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})
	target, sid := f.running(t, "worker")
	for _, q := range []string{
		`DELETE FROM pending_inputs`, // only the record-only fixture's old input
		`UPDATE sessions SET first_input='',agent_session_id='original-conversation'`,
		`UPDATE threads SET repo='repo'`,
		`INSERT INTO workspaces(id,thread_id,path,remote,state) VALUES(1,1,'fixture-worker','repo','ready')`,
		`INSERT INTO threads(id,scope,key,started_by,state,confined,kind,created_at,updated_at) VALUES(10,'s','supervisor','owner','running',1,'supervisor',0,0)`,
		`INSERT INTO sessions(id,thread_id,agent,agent_session_id,state,created_at,updated_at) VALUES(10,10,'fake','source-conversation','running',0,0)`,
	} {
		if _, err := f.core.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	x := &deferredFixture{fixture: f, target: target, sid: sid, repos: []string{"repo"}, caller: control.Caller{From: "supervisor:s", AsksIn: 10, Scopes: []string{"s"}, May: control.OpRead | control.OpSend, RefuseBusy: true, OrdinaryOnly: true, Mark: "supervisor", DeferInput: true}}
	x.reconstruct()
	return x
}

func (x *deferredFixture) reconstruct() {
	x.d = &DeferredInputs{Core: x.core, Policy: func(scope string, issuer control.ThreadID) (control.Caller, []string, error) {
		if scope != "s" || issuer != 10 {
			return control.Caller{}, nil, ErrForbidden
		}
		return x.caller, x.repos, nil
	}, Transport: func(context.Context, int64) (string, error) { return "original-run", nil }}
	x.core.Deferred = x.d
}

func (x *deferredFixture) api() control.API { return x.core.As(x.caller) }
func (x *deferredFixture) queue(t *testing.T) control.DeferredSend {
	t.Helper()
	r, e := x.api().DeferredSend(context.Background(), control.DeferredSendRequest{Action: "queue", Target: x.target, Key: "stable-key", Text: "bounded deferred instruction"})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (x *deferredFixture) receipt(t *testing.T, n int64) control.DeferredSend {
	t.Helper()
	r, e := x.api().DeferredSend(context.Background(), control.DeferredSendRequest{Action: "inspect", Number: n})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (x *deferredFixture) idle(t *testing.T) {
	x.agentEvent(t, x.sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
}
func (x *deferredFixture) freeze(t *testing.T) string {
	t.Helper()
	var input string
	if e := x.core.Store.DB.QueryRow(`SELECT input_ref FROM deferred_sends WHERE target=? ORDER BY id DESC LIMIT 1`, x.target).Scan(&input); e != nil {
		t.Fatal(e)
	}
	e := x.core.Store.Update(context.Background(), func(tx *store.Tx) error {
		_, e := x.d.FreezeTx(context.Background(), tx, x.sid, input, DeferredBatch{Run: "original-run", Data: [][]byte{[]byte("opaque complete fixture command")}, Command: [][]byte{[]byte("opaque complete fixture command")}, Digest: "fixture-digest"})
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	return input
}

func TestDeferredBusyToIdleKeepsSameInputAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	x := newDeferredFixture(t)
	if e := x.api().Send(ctx, x.target, "immediate", control.Input{Text: "immediate"}, control.Refuse); !errors.Is(e, ErrBusy) {
		t.Fatalf("immediate busy changed: %v", e)
	}
	r := x.queue(t)
	if r.State != "queued" {
		t.Fatal(r)
	}
	for range 2 {
		x.reconstruct()
		x.d.DispatchOnce(ctx)
	}
	if strings.Contains(x.runner.Calls(), "bounded deferred") {
		t.Fatal("busy queue was delivered")
	}
	if again := x.queue(t); again.Number != r.Number {
		t.Fatal("retry duplicated queue")
	}
	bad := control.DeferredSendRequest{Action: "queue", Target: x.target, Key: "stable-key", Text: "changed"}
	if _, e := x.api().DeferredSend(ctx, bad); e == nil {
		t.Fatal("changed payload reused key")
	}
	x.idle(t)
	x.reconstruct()
	x.d.DispatchOnce(ctx)
	x.reconstruct()
	x.d.DispatchOnce(ctx)
	if got := strings.Count(x.runner.Calls(), "deliver supervisor\nbounded deferred instruction"); got != 1 {
		t.Fatalf("deliveries %d: %s", got, x.runner.Calls())
	}
	if r = x.receipt(t, r.Number); r.State != "accepted" || r.Session != x.sid {
		t.Fatal(r)
	}
	input := x.freeze(t)
	if e := x.core.Store.Update(ctx, func(tx *store.Tx) error { return x.d.ResultTx(ctx, tx, x.sid, input, "written", 1234) }); e != nil {
		t.Fatal(e)
	}
	if got := x.receipt(t, r.Number).State; got != "handoff" {
		t.Fatalf("host receipt falsely delivered: %s", got)
	}
	x.agentEvent(t, x.sid, 3, agent.Event{Kind: agent.InputTaken, Input: input})
	if got := x.receipt(t, r.Number).State; got != "delivered" {
		t.Fatal(got)
	}
	if n := count(t, x.core.Store, "sessions"); n != 2 {
		t.Fatalf("replacement created: %d", n)
	}
}

func TestDeferredRefusesChangedAuthorityIdentityAndQuestions(t *testing.T) {
	changes := map[string]func(*testing.T, *deferredFixture){
		"watch":              func(_ *testing.T, x *deferredFixture) { x.caller.DeferInput = false; x.caller.May = control.OpRead },
		"act to propose":     func(_ *testing.T, x *deferredFixture) { x.caller.Propose = true },
		"repository revoked": func(_ *testing.T, x *deferredFixture) { x.repos = nil },
		"person revoked":     func(_ *testing.T, x *deferredFixture) { x.core.People = people.New(nil, "owner", nil) },
		"scope disabled": func(_ *testing.T, x *deferredFixture) {
			s := x.core.Scopes["s"]
			s.Enabled = false
			x.core.Scopes["s"] = s
		},
		"target closed": func(t *testing.T, x *deferredFixture) {
			_, e := x.core.Store.DB.Exec(`UPDATE sessions SET state='closed' WHERE id=?`, x.sid)
			if e != nil {
				t.Fatal(e)
			}
		},
		"conversation replaced": func(t *testing.T, x *deferredFixture) {
			_, e := x.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id='another-conversation' WHERE id=?`, x.sid)
			if e != nil {
				t.Fatal(e)
			}
		},
		"source replaced": func(t *testing.T, x *deferredFixture) {
			_, e := x.core.Store.DB.Exec(`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(11,10,'fake','running',0,0)`)
			if e != nil {
				t.Fatal(e)
			}
		},
		"question opened then resolved": func(t *testing.T, x *deferredFixture) {
			x.agentEvent(t, x.sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "new-question", Text: "new question"}})
			x.agentEvent(t, x.sid, 3, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "new-question"}})
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			x := newDeferredFixture(t)
			r := x.queue(t)
			change(t, x)
			x.reconstruct()
			x.d.DispatchOnce(context.Background())
			var state string
			if e := x.core.Store.DB.QueryRow(`SELECT state FROM deferred_sends WHERE id=?`, r.Number).Scan(&state); e != nil {
				t.Fatal(e)
			}
			if state != "refused" {
				t.Fatalf("unsafe queue survived: %s", state)
			}
			if strings.Contains(x.runner.Calls(), "bounded deferred") {
				t.Fatal("revoked queue reached runner")
			}
		})
	}
}

func TestDeferredCancellationLinearizesBeforeFreeze(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			x := newDeferredFixture(t)
			r := x.queue(t)
			x.idle(t)
			x.d.DispatchOnce(context.Background())
			if after {
				x.freeze(t)
			}
			got, e := x.api().DeferredSend(context.Background(), control.DeferredSendRequest{Action: "cancel", Number: r.Number})
			if e != nil {
				t.Fatal(e)
			}
			if after {
				if got.State != "handoff" || !strings.Contains(got.Reason, "too late") {
					t.Fatal(got)
				}
			} else {
				if got.State != "cancelled" {
					t.Fatal(got)
				}
				var input string
				x.core.Store.DB.QueryRow(`SELECT input_ref FROM deferred_sends WHERE id=?`, r.Number).Scan(&input)
				if e = x.core.RevalidatePending(context.Background(), x.sid, mustDeferredInt(t, input)); !errors.Is(e, ErrForbidden) {
					t.Fatalf("cancelled accepted slot could be delivered: %v", e)
				}
			}
		})
	}
}

func mustDeferredInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, e := fmt.Sscan(s, &n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestDeferredProposalApprovalQueuesInsteadOfSending(t *testing.T) {
	x := newDeferredFixture(t)
	x.caller.Propose = true
	r := x.queue(t)
	if r.State != "proposed" || r.Proposal == 0 {
		t.Fatal(r)
	}
	// Moving to act cannot auto-approve an already proposed instruction.
	x.caller.Propose = false
	x.reconstruct()
	x.d.DispatchOnce(context.Background())
	if got := x.receipt(t, r.Number).State; got != "proposed" {
		t.Fatal(got)
	}
	var handle control.RequestID
	if e := x.core.Store.DB.QueryRow(`SELECT handle FROM proposals WHERE id=?`, r.Proposal).Scan(&handle); e != nil {
		t.Fatal(e)
	}
	self := x.core.As(control.Caller{From: "supervisor:s", Scopes: []string{"s"}, May: control.OpAnswer})
	if e := self.Answer(context.Background(), handle, control.Answer{Allow: true}); !errors.Is(e, ErrForbidden) {
		t.Fatal("self approved", e)
	}
	approveTwice(t, x.fixture, handle)
	if got := x.receipt(t, r.Number).State; got != "queued" {
		t.Fatal(got)
	}
	if strings.Contains(x.runner.Calls(), "bounded deferred") {
		t.Fatal("approval sent into a busy turn")
	}
	x.idle(t)
	x.d.DispatchOnce(context.Background())
	if got := x.receipt(t, r.Number).State; got != "accepted" {
		t.Fatal(got)
	}
	if ev := eventsOf(t, x.fixture, 10, "proposal_answered"); len(ev) != 1 || !strings.Contains(ev[0], `"deferred":`) {
		t.Fatal(ev)
	}
}

func TestDeferredUnsupportedTransportCreatesNothing(t *testing.T) {
	x := newDeferredFixture(t)
	x.d.Transport = func(context.Context, int64) (string, error) { return "", control.ErrNotSupported }
	if _, e := x.api().DeferredSend(context.Background(), control.DeferredSendRequest{Action: "queue", Target: x.target, Key: "k", Text: "do it"}); !errors.Is(e, control.ErrNotSupported) {
		t.Fatal(e)
	}
	if count(t, x.core.Store, "deferred_sends") != 0 || count(t, x.core.Store, "proposals") != 0 {
		t.Fatal("unsupported transport created artifacts")
	}
}

func TestDeferredProposalDecisionPreservesOutcomeAndDecider(t *testing.T) {
	for _, decision := range []string{"refuse", "authority-changed", "target-changed"} {
		t.Run(decision, func(t *testing.T) {
			x := newDeferredFixture(t)
			x.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}, {Name: "owner", IDs: []string{"fixture:human"}}}, "owner", map[string][]string{"s": {"sv"}})
			x.caller.Propose = true
			r := x.queue(t)
			var handle control.RequestID
			if e := x.core.Store.DB.QueryRow(`SELECT handle FROM proposals WHERE id=?`, r.Proposal).Scan(&handle); e != nil {
				t.Fatal(e)
			}
			if decision == "authority-changed" {
				x.repos = nil
			}
			if decision == "target-changed" {
				if _, e := x.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id='different-conversation' WHERE id=?`, x.sid); e != nil {
					t.Fatal(e)
				}
			}
			decider := x.core.As(control.Caller{Principal: "owner", From: "fixture:human", Scopes: []string{"s"}, May: control.OpAnswer})
			if e := decider.Answer(context.Background(), handle, control.Answer{Allow: decision != "refuse"}); e != nil {
				t.Fatal(e)
			}
			var state, by, from string
			if e := x.core.Store.DB.QueryRow(`SELECT state,approved_by,approved_from FROM proposals WHERE id=?`, r.Proposal).Scan(&state, &by, &from); e != nil {
				t.Fatal(e)
			}
			want := proposalFailed
			if decision == "refuse" {
				want = proposalRefused
			}
			if state != want || by != "owner" || from != "fixture:human" {
				t.Fatalf("lost human decision: outcome=%s by=%s from=%s", state, by, from)
			}
			if got := x.receipt(t, r.Number).State; got != "refused" {
				t.Fatal(got)
			}
		})
	}
}

func TestDeferredLateQuestionAndRevocationBeforeFirstAttempt(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprint(frozen), func(t *testing.T) {
			x := newDeferredFixture(t)
			r := x.queue(t)
			x.idle(t)
			x.d.DispatchOnce(context.Background())
			var input string
			x.core.Store.DB.QueryRow(`SELECT input_ref FROM deferred_sends WHERE id=?`, r.Number).Scan(&input)
			if frozen {
				x.freeze(t)
			}
			x.agentEvent(t, x.sid, 3, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "late", Text: "a later question"}})
			x.agentEvent(t, x.sid, 4, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "late"}})
			var denied error
			if e := x.core.Store.Update(context.Background(), func(tx *store.Tx) error {
				if frozen {
					denied = x.d.BeforeAbsentTx(context.Background(), tx, x.sid, input)
				} else {
					denied = x.d.RevalidateTx(context.Background(), tx, x.sid, input)
				}
				if errors.Is(denied, ErrForbidden) {
					return nil
				}
				return denied
			}); e != nil {
				t.Fatal(e)
			}
			if !errors.Is(denied, ErrForbidden) {
				t.Fatal("a resolved new question did not revoke the unwritten attempt", denied)
			}
			if got := x.receipt(t, r.Number).State; got != "refused" && got != "failed" {
				t.Fatal(got)
			}
		})
	}
}

func TestDeferredCancelFreezeRace(t *testing.T) {
	for range 5 {
		x := newDeferredFixture(t)
		r := x.queue(t)
		x.idle(t)
		x.d.DispatchOnce(context.Background())
		var input string
		x.core.Store.DB.QueryRow(`SELECT input_ref FROM deferred_sends WHERE id=?`, r.Number).Scan(&input)
		var frozen bool
		var cancel control.DeferredSend
		var freezeErr, cancelErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			freezeErr = x.core.Store.Update(context.Background(), func(tx *store.Tx) error {
				_, e := x.d.FreezeTx(context.Background(), tx, x.sid, input, DeferredBatch{Run: "original-run", Data: [][]byte{[]byte("opaque")}, Command: [][]byte{[]byte("opaque")}, Digest: "fixture"})
				return e
			})
			frozen = freezeErr == nil
		}()
		go func() {
			defer wg.Done()
			cancel, cancelErr = x.api().DeferredSend(context.Background(), control.DeferredSendRequest{Action: "cancel", Number: r.Number})
		}()
		wg.Wait()
		if cancelErr != nil {
			t.Fatal(cancelErr)
		}
		if frozen {
			if cancel.State != "handoff" || !strings.Contains(cancel.Reason, "too late") {
				t.Fatalf("freeze won but cancellation claimed success: %+v", cancel)
			}
		} else if !errors.Is(freezeErr, ErrForbidden) || cancel.State != "cancelled" {
			t.Fatalf("cancel won but freeze was authorized: %v %+v", freezeErr, cancel)
		}
	}
}

func TestDeferredSleepKeepsOriginalConversation(t *testing.T) {
	x := newDeferredFixture(t)
	r := x.queue(t)
	x.idle(t)
	if e := x.core.Machine.Apply(context.Background(), x.sid, session.Input{Kind: session.IdleTimeout}); e != nil {
		t.Fatal(e)
	}
	x.reconstruct()
	x.d.DispatchOnce(context.Background())
	got := x.receipt(t, r.Number)
	if got.State != "accepted" || got.Session != x.sid || count(t, x.core.Store, "sessions") != 2 {
		t.Fatal(got)
	}
	var conversation, input string
	x.core.Store.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE id=?`, x.sid).Scan(&conversation)
	x.core.Store.DB.QueryRow(`SELECT input_ref FROM deferred_sends WHERE id=?`, r.Number).Scan(&input)
	if conversation != "original-conversation" || input != "first" || strings.Count(x.runner.Calls(), "resume supervisor\nbounded deferred instruction") != 1 {
		t.Fatal(conversation, input, x.runner.Calls())
	}
}

func TestDeferredReusedPendingSlotBindsCurrentOrigin(t *testing.T) {
	x := newDeferredFixture(t)
	ctx := context.Background()
	first := x.queue(t)
	x.idle(t)
	x.d.DispatchOnce(ctx)
	input := x.freeze(t)
	x.agentEvent(t, x.sid, 3, agent.Event{Kind: agent.InputTaken, Input: input})
	if _, e := x.core.Store.DB.Exec(`DELETE FROM pending_inputs WHERE session_id=?`, x.sid); e != nil {
		t.Fatal(e)
	}
	x.agentEvent(t, x.sid, 4, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	second, e := x.api().DeferredSend(ctx, control.DeferredSendRequest{Action: "queue", Target: x.target, Key: "second", Text: "second instruction"})
	if e != nil {
		t.Fatal(e)
	}
	x.d.DispatchOnce(ctx)
	var reused string
	x.core.Store.DB.QueryRow(`SELECT input_ref FROM deferred_sends WHERE id=?`, second.Number).Scan(&reused)
	if reused != input || first.Number == second.Number {
		t.Fatal("fixture did not reuse ordinary pending rowid")
	}
	b, bound, e := x.d.Input(ctx, x.sid, input)
	if e != nil || !bound || b.Run != "" {
		t.Fatalf("new input took old frozen command: %+v %v %v", b, bound, e)
	}
	x.freeze(t)
	x.agentEvent(t, x.sid, 5, agent.Event{Kind: agent.InputTaken, Input: input})
	if got := x.receipt(t, second.Number).State; got != "delivered" {
		t.Fatal(got)
	}
	if _, e = x.core.Store.DB.Exec(`DELETE FROM pending_inputs WHERE session_id=?`, x.sid); e != nil {
		t.Fatal(e)
	}
	x.agentEvent(t, x.sid, 6, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
	if e = x.owner.Send(ctx, x.target, "ordinary-next", control.Input{Text: "ordinary"}, control.Refuse); e != nil {
		t.Fatal(e)
	}
	if _, bound, e = x.d.Input(ctx, x.sid, input); e != nil || bound {
		t.Fatalf("old deferred receipt captured ordinary input: %v %v", bound, e)
	}
}

// The fake runner is record-only. Native CannotResume parsing is covered by
// the Codex resume_unknown capture; this verifies its durable core boundary
// without starting any host or replacement worker.
func TestDeferredCannotResumeRemainsFailedAcrossReconstruction(t *testing.T) {
	x := newDeferredFixture(t)
	ctx := context.Background()
	r := x.queue(t)
	x.idle(t)
	if e := x.core.Machine.Apply(ctx, x.sid, session.Input{Kind: session.IdleTimeout}); e != nil {
		t.Fatal(e)
	}
	x.d.DispatchOnce(ctx)
	conversation, bound, e := x.d.ResumeIdentity(ctx, x.sid)
	if e != nil || !bound || conversation != "original-conversation" {
		t.Fatal("same-session resume lost its original identity", conversation, bound, e)
	}
	for range 2 {
		x.reconstruct()
		if e := x.core.Store.Update(ctx, func(tx *store.Tx) error {
			prevent, e := x.d.PreventReplacementTx(ctx, tx, x.sid)
			if e == nil && !prevent {
				return errors.New("deferred cannot-resume allowed a replacement")
			}
			return e
		}); e != nil {
			t.Fatal(e)
		}
		if got := x.receipt(t, r.Number); got.State != "failed" || !strings.Contains(got.Reason, "no replacement") {
			t.Fatal(got)
		}
		if count(t, x.core.Store, "sessions") != 2 {
			t.Fatal("replacement session was created")
		}
	}
}
