package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/placement"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

type placementWorkspace struct {
	Workspaces
	made int
}

func (w *placementWorkspace) Make(ctx context.Context, id control.ThreadID, repo string, placed func(Placement) error) (string, error) {
	w.made++
	return w.Workspaces.Make(ctx, id, repo, placed)
}

type placementRunner struct {
	*fakeRunner
	store      *store.Store
	remoteMade int
	startError error
	uncertain  bool
}

func (r *placementRunner) Start(ctx context.Context, l Launch) error {
	if err := r.fakeRunner.Start(ctx, l); err != nil {
		return err
	}
	if r.startError != nil {
		if r.uncertain {
			if _, err := r.store.DB.ExecContext(ctx, `UPDATE sessions SET launch=2 WHERE id=?`, l.Session); err != nil {
				return err
			}
		}
		return r.startError
	}
	_, err := r.store.DB.ExecContext(ctx, `UPDATE sessions SET launch=0 WHERE id=?`, l.Session)
	return err
}
func (r *placementRunner) MakeRemoteWorkspace(context.Context, int64, string) (string, error) {
	r.remoteMade++
	return "owned-workspace-handle", nil
}

func placementFixture(t *testing.T) (*fixture, *placementWorkspace, *placementRunner, *bool) {
	t.Helper()
	f := newFixture(t, fakeOwner{})
	w := &placementWorkspace{Workspaces: f.core.Workspaces}
	r := &placementRunner{fakeRunner: f.runner, store: f.core.Store}
	f.core.Workspaces = w
	f.core.Runner = r
	available := true
	f.core.Placement = &MachinePlacement{Machines: map[string]PlacementMachine{"local": {Limits: placement.Limits{MaxResident: 1}}}, Observe: func(context.Context, string, string) (placement.Observation, error) {
		now := time.Now()
		return placement.Observation{Ready: available, Reconciled: true, Known: true, Installed: true, LoggedIn: true, At: now, HealthAt: now, ValidFor: 10 * time.Second, Memory: 1024, Disk: 2048}, nil
	}}
	return f, w, r, &available
}

func TestPlacementConcurrentExplicitStartsReserveBeforeWorkspace(t *testing.T) {
	f, w, _, _ := placementFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.owner.Start(ctx, control.StartRequest{Scope: "s", Thread: fmt.Sprint("link", i), Key: fmt.Sprint(i), Machine: "local"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	refused := 0
	for err := range errs {
		if errors.Is(err, session.ErrRefused) {
			refused++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if refused != 1 || w.made != 1 {
		t.Fatalf("refusals=%d workspaces=%d", refused, w.made)
	}
	rs, err := f.core.Store.MachineReservations(ctx)
	if err != nil || len(rs) != 1 {
		t.Fatal(rs, err)
	}
}

func TestPlacementDurableWaitRepeatOrderAndCancel(t *testing.T) {
	f, w, r, available := placementFixture(t)
	ctx := context.Background()
	*available = false
	var ids []control.ThreadID
	for i := range 3 {
		request := control.StartRequest{Scope: "s", Thread: fmt.Sprint("link", i), Key: fmt.Sprint(i), Input: control.Input{Text: fmt.Sprint("input", i)}}
		id, err := f.owner.Start(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.owner.Start(ctx, request)
		if err != nil || again != id {
			t.Fatal(id, again, err)
		}
		ids = append(ids, id)
	}
	if w.made != 0 || r.Calls() != "" {
		t.Fatalf("waiting start had effects: %d %s", w.made, r.Calls())
	}
	for _, id := range ids {
		d, err := f.owner.Thread(ctx, id, 0, 0)
		if err != nil || !d.PlacementPending || d.Machine != "" {
			t.Fatal(d, err)
		}
	}
	// A fresh core instance reads the same durable rows. Recovery must not
	// turn accepted unplaced starts into default-local launches.
	c := &Core{Store: f.core.Store, Machine: f.core.Machine, Scopes: f.core.Scopes, Owners: f.core.Owners, People: f.core.People, Workspaces: w, Runner: r, Agent: f.core.Agent, Log: f.core.Log, Placement: f.core.Placement}
	if err := c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if w.made != 0 {
		t.Fatal("restart cloned a pending workspace")
	}
	*available = true
	if err := c.DispatchPlacements(ctx); err != nil {
		t.Fatal(err)
	}
	if w.made != 1 || r.Calls() != "start input0" {
		t.Fatalf("oldest first: %d %q", w.made, r.Calls())
	}
	closeAPI := c.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: control.OpClose})
	if err := closeAPI.CloseThread(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if w.made != 1 {
		t.Fatal("cancel created a workspace")
	}
	ps, err := c.Store.PendingPlacements(ctx)
	if err != nil || len(ps) != 1 || ps[0].Thread != int64(ids[2]) {
		t.Fatal(ps, err)
	}
	var state string
	if err := c.Store.DB.QueryRow(`SELECT state FROM threads WHERE id=?`, int64(ids[1])).Scan(&state); err != nil || state != "closed" {
		t.Fatal(state, err)
	}
}

func TestPlacementExplicitRefusalHasNoFallbackEffects(t *testing.T) {
	f, w, r, _ := placementFixture(t)
	ctx := context.Background()
	for _, key := range []string{"missing", "forbidden"} {
		f.core.Placement.Machines["forbidden"] = PlacementMachine{}
		sc := f.core.Scopes["s"]
		sc.Machines = []string{"local"}
		f.core.Scopes["s"] = sc
		id, err := f.owner.Start(ctx, control.StartRequest{Scope: "s", Thread: key, Key: key, Machine: key})
		if id != 0 || !errors.Is(err, session.ErrRefused) {
			t.Fatal(id, err)
		}
	}
	if w.made != 0 || r.Calls() != "" {
		t.Fatal("explicit refusal launched locally")
	}
	var n int
	f.core.Store.DB.QueryRow(`SELECT count(*) FROM threads`).Scan(&n)
	if n != 0 {
		t.Fatal(n)
	}
}

func TestPlacementLostRemoteLaunchKeepsReservation(t *testing.T) {
	f, w, r, _ := placementFixture(t)
	ctx := context.Background()
	if err := f.core.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "worker", Root: "/worker/root"}}); err != nil {
		t.Fatal(err)
	}
	f.core.Placement.Machines = map[string]PlacementMachine{"build": {Limits: placement.Limits{MaxResident: 1}}}
	r.startError = errors.New("launch acknowledgment lost")
	id, err := f.owner.Start(ctx, control.StartRequest{Scope: "s", Thread: "remote", Key: "remote", Machine: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if w.made != 0 || r.remoteMade != 1 {
		t.Fatalf("wrong workspace authority: local=%d remote=%d", w.made, r.remoteMade)
	}
	var state, machine string
	var launch int
	if err := f.core.Store.DB.QueryRow(`SELECT t.state,t.machine,s.launch FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE t.id=?`, int64(id)).Scan(&state, &machine, &launch); err != nil || state != "creating" || machine != "build" || launch != 1 {
		t.Fatal(state, machine, launch, err)
	}
	if _, err := f.owner.Start(ctx, control.StartRequest{Scope: "s", Thread: "second", Key: "second", Machine: "build"}); !errors.Is(err, session.ErrRefused) {
		t.Fatal("unknown launch gave up room", err)
	}
	rs, err := f.core.Store.MachineReservations(ctx)
	if err != nil || len(rs) != 1 {
		t.Fatal(rs, err)
	}
}

func TestPlacementDispatchRechecksAuthorization(t *testing.T) {
	for _, change := range []string{"person", "scope", "grant", "agent", "empty scope machines"} {
		t.Run(change, func(t *testing.T) {
			f, w, _, available := placementFixture(t)
			ctx := context.Background()
			*available = false
			id, err := f.owner.Start(ctx, control.StartRequest{Scope: "s", Thread: "pending", Key: "pending"})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "person":
				f.core.Placement.Caller = func(context.Context, control.Caller, string, string) (control.Caller, error) {
					return control.Caller{Principal: "stranger", Scopes: []string{"s"}, May: control.OpStart}, nil
				}
			case "scope":
				sc := f.core.Scopes["s"]
				sc.Enabled = false
				f.core.Scopes["s"] = sc
			case "grant":
				f.core.Placement.Caller = func(context.Context, control.Caller, string, string) (control.Caller, error) {
					return control.Caller{}, control.ErrForbidden
				}
			case "agent":
				f.core.Agents = map[string]AgentDefaults{}
			case "empty scope machines":
				sc := f.core.Scopes["s"]
				sc.Machines = []string{}
				f.core.Scopes["s"] = sc
			}
			*available = true
			if err := f.core.DispatchPlacements(ctx); err != nil {
				t.Fatal(err)
			}
			if w.made != 0 {
				t.Fatal("revoked start made workspace")
			}
			var state string
			f.core.Store.DB.QueryRow(`SELECT state FROM threads WHERE id=?`, int64(id)).Scan(&state)
			if state != "closed" {
				t.Fatal(state)
			}
		})
	}
}

func (r *placementRunner) Wake(ctx context.Context, sid int64, p control.Principal) error {
	if r.startError != nil {
		return r.startError
	}
	return r.fakeRunner.Wake(ctx, sid, p)
}
func TestPlacementAttachmentWaitRetainsProvenanceAndLaunch(t *testing.T) {
	f, _, r, _ := placementFixture(t)
	f.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:owner-id"}}}, "owner", nil)
	id, sid := f.asleep(t, "attachment-wait")
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET agent_session_id='conversation' WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	r.startError = ErrPlacementWait
	caller := control.Caller{Principal: "owner", From: "chat:owner-id", Via: "dashboard", Scopes: []string{"s"}, May: control.OpAttach}
	_, err := f.core.As(caller).AttachTerminal(context.Background(), id)
	if !errors.Is(err, ErrPlacementWait) {
		t.Fatal(err)
	}
	var raw, state string
	var launch int
	if err := f.core.Store.DB.QueryRow(`SELECT first_input,state,launch FROM sessions WHERE id=?`, sid).Scan(&raw, &state, &launch); err != nil {
		t.Fatal(err)
	}
	var first FirstInput
	if err := json.Unmarshal([]byte(raw), &first); err != nil {
		t.Fatal(err)
	}
	if state != "creating" || launch != 1 || first.AcceptedOperation != control.OpAttach || first.From != caller.From || first.Via != caller.Via || !first.NoInput {
		t.Fatalf("lost attachment wake: %s phase=%d %s", state, launch, raw)
	}
}
func TestPlacementRecoveryRechecksTheAcceptedOperationGrant(t *testing.T) {
	f, _, _, _ := placementFixture(t)
	for _, tc := range []struct {
		op   control.Ops
		name string
	}{{control.OpStart, "Start"}, {control.OpSend, "Send"}, {control.OpAttach, "AttachTerminal"}} {
		f.core.Placement.Caller = func(_ context.Context, old control.Caller, scope, repo string) (control.Caller, error) {
			return control.Caller{Principal: old.Principal, Scopes: []string{scope}, May: tc.op, Limit: &control.Limit{Operations: []string{tc.name}}}, nil
		}
		if err := f.core.recoveryPlacementAuthority(context.Background(), "s", "repo", FirstInput{AcceptedOperation: tc.op, Principal: "owner"}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

func TestPlacementLocalLostStartKeepsCommittedRecovery(t *testing.T) {
	f, _, r, _ := placementFixture(t)
	r.startError = errors.New("lost host readiness reply")
	r.uncertain = true
	id, err := f.owner.Start(context.Background(), control.StartRequest{Scope: "s", Thread: "lost-local", Key: "lost-local", Machine: "local"})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	var phase int
	if err := f.core.Store.DB.QueryRow(`SELECT state,launch FROM sessions WHERE thread_id=?`, int64(id)).Scan(&state, &phase); err != nil {
		t.Fatal(err)
	}
	if state != "creating" || phase != 2 {
		t.Fatal("uncertain local host lost recovery", state, phase)
	}
	reservations, err := f.core.Store.MachineReservations(context.Background())
	if err != nil || len(reservations) != 1 {
		t.Fatal(reservations, err)
	}
	r.startError = nil
	if err := f.core.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Store.DB.QueryRow(`SELECT launch FROM sessions WHERE thread_id=?`, int64(id)).Scan(&phase); err != nil || phase != 0 {
		t.Fatal(phase, err)
	}
}
func TestPlacementProposalKeepsMachineAndRechecksApproval(t *testing.T) {
	f, proposer, asksIn, _ := proposalFixture(t)
	ctx := context.Background()
	before := count(t, f.core.Store, "threads")
	_, err := proposer.Start(ctx, control.StartRequest{Scope: "s", Key: "machine-proposal", Repo: "repo", Machine: "build", Input: control.Input{Text: "work"}})
	proposalOf(t, err)
	ev := eventsOf(t, f, asksIn, "proposal_made")
	if len(ev) != 1 || !strings.Contains(ev[0], `"machine":"build"`) {
		t.Fatal(ev)
	}
	var raw string
	if err := f.core.Store.DB.QueryRow(`SELECT call FROM proposals ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved proposalCall
	if err := json.Unmarshal([]byte(raw), &saved); err != nil || saved.Machine != "build" {
		t.Fatal(raw, err)
	}
	f.core.Placement = &MachinePlacement{Machines: map[string]PlacementMachine{}}
	d, err := f.owner.Thread(ctx, asksIn, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatal(d, err)
	}
	if err := f.owner.Answer(ctx, d.Requests[0].ID, control.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if count(t, f.core.Store, "threads") != before {
		t.Fatal("approval fell back after explicit machine removal")
	}
	outcomes := eventsOf(t, f, asksIn, "proposal_answered")
	if len(outcomes) != 1 || !strings.Contains(outcomes[0], `"outcome":"failed"`) {
		t.Fatal(outcomes)
	}
}

func TestPlacementWorkspaceRecordPreservesAcceptedOperation(t *testing.T) {
	f, _, r, _ := placementFixture(t)
	ctx := context.Background()
	id, err := f.owner.Start(ctx, control.StartRequest{Scope: "s", Thread: "record-grant", Key: "record-grant"})
	if err != nil {
		t.Fatal(err)
	}
	var sid int64
	f.core.Store.DB.QueryRow(`SELECT id FROM sessions WHERE thread_id=?`, int64(id)).Scan(&sid)
	first, _ := json.Marshal(FirstInput{AcceptedOperation: control.OpSend, Principal: "owner", NoInput: true})
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET first_input=?,launch=1 WHERE id=?`, string(first), sid); err != nil {
		t.Fatal(err)
	}
	if err := f.core.launch(ctx, Launch{AcceptedOperation: control.OpSend, Session: sid, Thread: id, Agent: f.core.Agent, Principal: "owner", NoInput: true}, "repo"); err != nil {
		t.Fatal(err)
	}
	var raw string
	f.core.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=?`, sid).Scan(&raw)
	var got FirstInput
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.AcceptedOperation != control.OpSend || !got.NoInput {
		t.Fatal("workspace rewrite lost accepted operation", raw)
	}
	_ = r
}
