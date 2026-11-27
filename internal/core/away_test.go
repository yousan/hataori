package core

import (
	"context"
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
