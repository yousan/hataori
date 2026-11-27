package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/placement"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Core-only records use a fake runner and never own or inspect a process.
func awayFixture(t *testing.T) (*fixture, control.ThreadID, int64, *bool) {
	t.Helper()
	f, _, _, _ := placementFixture(t)
	ctx := context.Background()
	if err := f.core.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
		t.Fatal(err)
	}
	f.core.Placement.Machines = map[string]PlacementMachine{"build": {Limits: placement.Limits{MaxResident: 4}}}
	id, sid := f.running(t, "worker")
	if _, err := f.core.Store.DB.Exec(`INSERT INTO session_machine_runs(session_id,machine,run,workspace,agent_config,phase) VALUES(?,'build',?,'handle','default','running')`, sid, fmt.Sprint("run-", sid)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET spec='{}',launch=0 WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.ObserveConnection(ctx, "build", false, "", "transport", false, time.Millisecond, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	revoked := false
	f.core.Away = &MachineAway{Status: func(ctx context.Context, key string) (control.MachineAvailability, error) {
		a, err := f.core.Store.Availability(ctx, key)
		return control.MachineAvailability{State: a.State, At: time.UnixMilli(a.FirstFailure), Reason: a.Reason}, err
	}, Caller: func(_ context.Context, old control.Caller, scope, repo string) (control.Caller, error) {
		if revoked {
			return control.Caller{}, ErrForbidden
		}
		return control.Caller{Principal: old.Principal, From: old.From, Via: old.Via, Scopes: []string{scope}, May: all}, nil
	}}
	f.runner.calls = nil
	return f, id, sid, &revoked
}

func awayReady(t *testing.T, f *fixture, sid int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.core.Store.ObserveConnection(ctx, "build", true, "new-generation", "", false, time.Second, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Store.AcknowledgeCatchup(ctx, sid, "new-generation"); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Store.FinishCatchup(ctx, "build", "new-generation", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func restartAwayCore(t *testing.T, f *fixture) {
	t.Helper()
	old := f.core
	if err := old.Store.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &session.Machine{Store: st}
	f.runner.machine = m
	f.core = &Core{Store: st, Machine: m, Runner: old.Runner, Scopes: old.Scopes, People: old.People, Owners: old.Owners, Agent: old.Agent, Workspaces: old.Workspaces, Log: old.Log, Placement: old.Placement, Away: old.Away}
	f.owner = f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all})
}

func TestAwayHeldInputStopRestartOrderAndUncheckedRefusals(t *testing.T) {
	f, id, sid, _ := awayFixture(t)
	ctx := context.Background()
	var turn int64
	f.core.Store.DB.QueryRow(`SELECT max(id) FROM turns WHERE session_id=?`, sid).Scan(&turn)
	for i := 0; i < 2; i++ {
		if err := f.owner.Send(ctx, id, "held-input", control.Input{Text: "later"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		if err := f.owner.Stop(ctx, id, turn); !errors.Is(err, ErrActionHeld) {
			t.Fatalf("stop did not report pending: %v", err)
		}
	}
	if n := count(t, f.core.Store, "machine_held_actions"); n != 2 {
		t.Fatalf("held=%d", n)
	}
	var notices int
	f.core.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind='machine_action'`).Scan(&notices)
	if notices != 1 {
		t.Fatalf("notices=%d", notices)
	}
	if len(f.runner.calls) != 0 {
		t.Fatalf("away dispatched %v", f.runner.calls)
	}
	admin := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpClose | control.OpRemove | control.OpReopen | control.OpAttach})
	for name, call := range map[string]func() error{"close": func() error { return admin.CloseThread(ctx, id) }, "remove": func() error { return admin.RemoveThread(ctx, id) }, "reopen": func() error { _, err := admin.ReopenThread(ctx, id); return err }, "attach": func() error { _, err := admin.(*api).AttachWorker(ctx, id); return err }} {
		if err := call(); !errors.Is(err, ErrMachineUnchecked) {
			t.Errorf("%s unchecked: %v", name, err)
		}
	}
	restartAwayCore(t, f)
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.calls) != 0 {
		t.Fatalf("restart dispatched before catch-up %v", f.runner.calls)
	}
	awayReady(t, f, sid)
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.calls) != 2 || f.runner.calls[0] != "deliver later" || !strings.HasPrefix(f.runner.calls[1], "interrupt") {
		t.Fatalf("held order %v", f.runner.calls)
	}
	if err := f.owner.Send(ctx, id, "held-input", control.Input{Text: "later"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.calls) != 2 {
		t.Fatalf("repeated held key dispatched again %v", f.runner.calls)
	}
	var slots int
	f.core.Store.DB.QueryRow(`SELECT count(*) FROM machine_reservations`).Scan(&slots)
	if slots != 1 {
		t.Fatalf("slot lost %d", slots)
	}
}

func TestAwayQueuedActionsRecheckAuthorityScopeAndOriginalTargets(t *testing.T) {
	for _, mode := range []string{"person revoked", "scope disabled", "turn changed", "question withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			f, id, sid, revoked := awayFixture(t)
			ctx := context.Background()
			if mode == "question withdrawn" {
				f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "original", Text: "choice?"}})
				request := session.RequestHandle(sid, "original")
				if err := f.owner.Answer(ctx, request, control.Answer{Text: "old answer"}); err != nil {
					t.Fatal(err)
				}
				if err := f.owner.Answer(ctx, request, control.Answer{Text: "old answer"}); err != nil {
					t.Fatal(err)
				}
				f.agentEvent(t, sid, 3, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "original"}})
			} else {
				if err := f.owner.Stop(ctx, id, 0); !errors.Is(err, ErrActionHeld) {
					t.Fatal(err)
				}
			}
			restartAwayCore(t, f)
			switch mode {
			case "person revoked":
				*revoked = true
			case "scope disabled":
				s := f.core.Scopes["s"]
				s.Enabled = false
				f.core.Scopes["s"] = s
			case "turn changed":
				f.agentEvent(t, sid, 2, agent.Event{Kind: agent.TurnEnded, Status: agent.Completed})
				f.agentEvent(t, sid, 3, agent.Event{Kind: agent.TurnStarted, Input: "autonomous"})
			}
			awayReady(t, f, sid)
			if err := f.core.DispatchHeld(ctx); err != nil {
				t.Fatal(err)
			}
			if len(f.runner.calls) != 0 {
				t.Fatalf("stale/revoked action dispatched: %v", f.runner.calls)
			}
			var state string
			f.core.Store.DB.QueryRow(`SELECT state FROM machine_held_actions`).Scan(&state)
			if state != "refused" {
				t.Fatalf("refusal invisible %q", state)
			}
		})
	}
}

func TestAwayHeldSendBindsOriginalTargetInsideAcceptance(t *testing.T) {
	for _, change := range []string{"new question", "new run"} {
		t.Run(change, func(t *testing.T) {
			f, id, sid, _ := awayFixture(t)
			ctx := context.Background()
			f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "q1", Text: "first?"}})
			if err := f.owner.Send(ctx, id, "original-answer", control.Input{Text: "for q1"}, control.Deliver); err != nil {
				t.Fatal(err)
			}
			awayReady(t, f, sid)
			baseline := count(t, f.core.Store, "pending_inputs")
			f.core.Hooks = &countHooks{}
			f.core.stateRead = func() {
				f.core.stateRead = nil
				if change == "new question" {
					f.agentEvent(t, sid, 3, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "q1"}})
					f.agentEvent(t, sid, 4, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "q2", Text: "second?"}})
				} else {
					if _, err := f.core.Store.DB.Exec(`UPDATE session_machine_runs SET run='replacement-run' WHERE session_id=?`, sid); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.core.DispatchHeld(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			f.core.Store.DB.QueryRow(`SELECT state FROM machine_held_actions`).Scan(&state)
			if state != "refused" || len(f.runner.calls) != 0 || count(t, f.core.Store, "pending_inputs") != baseline {
				t.Fatalf("retargeted held answer: %s %v", state, f.runner.calls)
			}
		})
	}
}

func TestAwayClosedCleanupPreservesRemainingActiveAge(t *testing.T) {
	f, id, sid, _ := awayFixture(t)
	ctx := context.Background()
	now := time.Now()
	f.core.Machine.Now = func() time.Time { return now }
	closed := now.Add(-3 * time.Hour)
	for _, v := range []struct {
		q    string
		args []any
	}{
		{`UPDATE threads SET state='closed',updated_at=? WHERE id=?`, []any{closed.UnixMilli(), id}},
		{`UPDATE sessions SET state='closed' WHERE id=?`, []any{sid}},
		{`DELETE FROM session_pauses WHERE session_id=?`, []any{sid}},
		{`INSERT INTO session_pauses(session_id,began_at,ended_at) VALUES(?,?,?)`, []any{sid, closed.Add(30 * time.Minute).UnixMilli(), now.UnixMilli()}},
	} {
		if _, err := f.core.Store.DB.Exec(v.q, v.args...); err != nil {
			t.Fatal(err)
		}
	}
	awayReady(t, f, sid)
	rm := &fakeRemover{}
	f.core.Remover = rm
	if n, err := f.core.RemoveClosed(ctx, time.Hour); err != nil || n != 0 || rm.Asked() != 0 {
		t.Fatalf("cleanup counted absence: %d %v asks=%d", n, err, rm.Asked())
	}
	now = now.Add(31 * time.Minute)
	if n, err := f.core.RemoveClosed(ctx, time.Hour); err != nil || n != 0 || len(f.eventsOf(t, id, KindRemovalRefused)) != 1 {
		t.Fatalf("remaining cleanup budget did not attempt the worker proof: %d %v", n, err)
	}
}

func TestAwayAcceptedQueueReceiptCommitsBeforeRunnerWrite(t *testing.T) {
	f, id, sid, _ := awayFixture(t)
	ctx := context.Background()
	if err := f.owner.Send(ctx, id, "receipt-boundary", control.Input{Text: "once"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	awayReady(t, f, sid)
	f.runner.deliverErr = ErrMachineUnchecked
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	f.core.Store.DB.QueryRow(`SELECT state FROM machine_held_actions`).Scan(&state)
	if state != "dispatched" {
		t.Fatalf("durable acceptance lost to transport error: %s", state)
	}
	before := count(t, f.core.Store, "pending_inputs")
	restartAwayCore(t, f)
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, f.core.Store, "pending_inputs") != before || len(f.runner.calls) != 0 {
		t.Fatalf("accepted API replayed: %v", f.runner.calls)
	}
	if n := len(f.eventsOf(t, id, "machine_action")); n != 2 {
		t.Fatalf("expected one pending notice and one dispatch receipt: %d", n)
	}
}

func TestAwayRevokedInitialInputKeepsHostMonitoringAndAudit(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			f, id, sid, revoked := awayFixture(t)
			ctx := context.Background()
			var turn int64
			f.core.Store.DB.QueryRow(`SELECT max(id) FROM turns WHERE session_id=?`, sid).Scan(&turn)
			first := FirstInput{Principal: "owner", Text: "never dispatch revoked initial input", Key: "initial", Context: "owed context", Sent: sent}
			if sent {
				first.TurnID = turn
			}
			raw, _ := json.Marshal(first)
			if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET first_input=?,launch=0 WHERE id=?`, string(raw), sid); err != nil {
				t.Fatal(err)
			}
			*revoked = true
			if allowed, err := f.core.RevalidatePlacementLaunch(ctx, sid); err != nil || !allowed {
				t.Fatalf("owned host monitoring canceled: %v %v", allowed, err)
			}
			var rejection error
			if err := f.core.Store.Update(ctx, func(tx *store.Tx) error {
				rejection = f.core.RevalidateInitial(ctx, tx, sid)
				if errors.Is(rejection, ErrForbidden) {
					return nil
				}
				return rejection
			}); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(rejection, ErrForbidden) {
				t.Fatalf("initial authority not rechecked: %v", rejection)
			}
			var input, context string
			var launch int
			f.core.Store.DB.QueryRow(`SELECT first_input,launch,context FROM sessions WHERE id=?`, sid).Scan(&input, &launch, &context)
			if input != "" || launch != 0 || context != "owed context" || count(t, f.core.Store, "machine_rejected_inputs") != 1 {
				t.Fatalf("rejection lost monitoring/audit/context: %q %d %q", input, launch, context)
			}
			if sent {
				var ended any
				f.core.Store.DB.QueryRow(`SELECT ended_at FROM turns WHERE id=?`, turn).Scan(&ended)
				if ended == nil {
					t.Fatal("bound caller turn left running after rejection")
				}
			}
			restartAwayCore(t, f)
			*revoked = false
			if err := f.core.Store.Update(ctx, func(tx *store.Tx) error { return f.core.RevalidateInitial(ctx, tx, sid) }); err != nil {
				t.Fatal(err)
			}
			if len(f.eventsOf(t, id, "machine_action")) != 1 || count(t, f.core.Store, "machine_rejected_inputs") != 1 {
				t.Fatal("regrant revived rejected first input or duplicated refusal")
			}
		})
	}
}

func TestAwayAPIAvailabilityPreservesHistoricalTimestamp(t *testing.T) {
	f, id, sid, _ := awayFixture(t)
	var at int64
	f.core.Store.DB.QueryRow(`SELECT updated_at FROM sessions WHERE id=?`, sid).Scan(&at)
	detail, err := f.owner.Thread(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := detail.Availability
	if a == nil || a.State == "ready" || a.LastKnownState != control.Running || a.LastKnownAt == nil || a.LastKnownAt.UnixMilli() != at {
		t.Fatalf("availability overlay lost history: %+v", a)
	}
}
