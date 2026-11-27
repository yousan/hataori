package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/placement"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// MachinePlacement contains configured policy and observations of installations
// already reconciled by the daemon. Observe performs no work inside a DB tx.
type MachinePlacement struct {
	Status   func(string) control.MachineAvailability
	Machines map[string]PlacementMachine
	Observe  func(context.Context, string, string) (placement.Observation, error)
	// Caller rebuilds the accepted caller from current daemon grants. An error
	// cancels an unplaced start; stored permissions are never replayed as grants.
	Caller func(context.Context, control.Caller, string, string) (control.Caller, error)
}

type PlacementMachine struct {
	placement.Limits
	Agents []string
}

// PlacementLaunchCanceled is a terminal dispatch marker, distinct from a
// completed launch (zero). Reconnect must never follow its unsent first input.
// The host and reservation remain owned until positive end reconciliation.
const PlacementLaunchCanceled = 3

var ErrPlacementWait = errors.New("waiting for room on the recorded machine")

// waitForMachine retains both the committed launch and its reservation. An
// unavailable reply can follow a successful remote effect, so it proves no end.
func (c *Core) waitForMachine(ctx context.Context, sid int64, cause error) error {
	if c.Placement == nil {
		return cause
	}
	c.Log.Warn("session waits on its recorded machine", "session", sid, "err", cause)
	_, err := c.Store.DB.ExecContext(ctx, `UPDATE sessions SET io_error=?,launch=CASE WHEN launch=0 THEN 1 ELSE launch END WHERE id=? AND state='creating'`, ErrPlacementWait.Error(), sid)
	return err
}

// RunPlacements also retries already committed fixed-machine launches. Their
// durable keys and receipts remain the runner's responsibility.
func (c *Core) RunPlacements(ctx context.Context) {
	if c.Placement == nil {
		return
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		if err := c.DispatchPlacements(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("dispatching placements", "err", err)
		}
		if err := c.acknowledgeWorkerTrash(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("acknowledging worker trash", "err", err)
		}
		if err := c.Recover(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("recovering machine launches", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type placementRequest struct {
	Caller control.Caller `json:"caller"`
}

func (c *Core) recoveryPlacementAuthority(ctx context.Context, scope, repo string, f FirstInput) error {
	if c.Placement == nil {
		return nil
	}
	op := f.AcceptedOperation
	if op == 0 {
		op = control.OpStart
		if f.Wake {
			op = control.OpSend
		}
		if f.NoInput {
			op = control.OpAttach
		}
	}
	old := control.Caller{From: f.From, Principal: f.Principal, Via: f.Via}
	var fresh control.Caller
	var err error
	if c.Placement.Caller != nil {
		fresh, err = c.Placement.Caller(ctx, old, scope, repo)
	} else {
		fresh = control.Caller{From: old.From, Principal: old.Principal, Scopes: []string{scope}, May: op}
	}
	if err != nil {
		return err
	}
	name := "Start"
	switch op {
	case control.OpSend:
		name = "Send"
	case control.OpAttach:
		name = "AttachTerminal"
	}
	return c.As(fresh).(*api).act(op, name, scope)
}

func (c *Core) placementObservations(ctx context.Context, agent string) map[string]placement.Observation {
	out := map[string]placement.Observation{}
	if c.Placement == nil || c.Placement.Observe == nil {
		return out
	}
	for key := range c.Placement.Machines {
		if o, err := c.Placement.Observe(ctx, key, agent); err == nil {
			out[key] = o
		}
	}
	return out
}

// ReconcileLocalPlacements releases only positively ended owned holders. A
// failed/asleep session row alone cannot prove its process stopped.
func (c *Core) ReconcileLocalPlacements(ctx context.Context) error {
	probe, ok := c.Runner.(interface {
		PlacementEnded(context.Context, int64) (bool, error)
	})
	if !ok {
		return nil
	}
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT r.thread_id,r.operation,s.id FROM machine_reservations r JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=r.thread_id) WHERE r.machine='local' AND s.state IN ('asleep','closed','failed','removed')`)
	if err != nil {
		return err
	}
	type ended struct {
		thread, sid int64
		operation   string
	}
	var found []ended
	for rows.Next() {
		var x ended
		if err := rows.Scan(&x.thread, &x.operation, &x.sid); err != nil {
			rows.Close()
			return err
		}
		found = append(found, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range found {
		if ended, err := probe.PlacementEnded(ctx, x.sid); err != nil {
			return err
		} else if ended {
			if _, err := c.Store.DB.ExecContext(ctx, `DELETE FROM machine_reservations WHERE thread_id=? AND operation=? AND EXISTS(SELECT 1 FROM sessions s WHERE s.id=? AND s.state IN ('asleep','closed','failed','removed') AND s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=s.thread_id))`, x.thread, x.operation, x.sid); err != nil {
				return err
			}
		}
	}
	return nil
}

// choosePlacement runs with the reservation transaction. Inventory rows remain
// durable during outages. A reservation and the runs it covers consume room
// once; any additional possibly live run consumes its own slot.
func (c *Core) choosePlacement(ctx context.Context, tx *store.Tx, scope, agent, explicit string, except int64, observations map[string]placement.Observation) (string, string, error) {
	var candidates []placement.Candidate
	for key, cfg := range c.Placement.Machines {
		var configured bool
		if err := tx.QueryRowContext(ctx, `SELECT configured FROM machines WHERE key=?`, key).Scan(&configured); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		sc := c.Scopes[scope]
		x := placement.Candidate{Key: key, Configured: configured, Allowed: sc.Machines == nil || slices.Contains(sc.Machines, key),
			AgentAllowed: cfg.Agents == nil || slices.Contains(cfg.Agents, agent), Limits: cfg.Limits, Observation: observations[key]}
		// Every main reservation counts until a positive end observation. Known
		// live sessions from before placement also count, including confined ones.
		err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(resident),0),coalesce(sum(starting),0) FROM (
			SELECT t.id, max(1,(SELECT count(*) FROM machine_inventory i WHERE i.machine=t.machine AND i.thread_id=t.id AND i.state NOT IN ('ended','gone'))) resident, max(s.state='creating',(SELECT count(*) FROM machine_inventory i WHERE i.machine=t.machine AND i.thread_id=t.id AND i.state IN ('reserved','unknown'))) starting FROM threads t
			JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id)
			WHERE t.machine=? AND t.id!=? AND ((s.state IN ('idle','running','waiting') OR s.state='creating' AND t.confined!=0)
			 OR EXISTS(SELECT 1 FROM machine_reservations r WHERE r.thread_id=t.id)) GROUP BY t.id
			UNION ALL
			SELECT i.thread_id, 1, i.state IN ('reserved','unknown') FROM machine_inventory i
			WHERE i.machine=? AND i.thread_id!=? AND i.state NOT IN ('ended','gone')
			AND NOT EXISTS(SELECT 1 FROM threads t JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id)
			 WHERE t.id=i.thread_id AND ((s.state IN ('idle','running','waiting') OR s.state='creating' AND t.confined!=0) OR EXISTS(SELECT 1 FROM machine_reservations r WHERE r.thread_id=t.id)))
		)`, key, except, key, except).Scan(&x.Resident, &x.Starting)
		if err != nil {
			return "", "", err
		}
		candidates = append(candidates, x)
	}
	key, reasons := placement.Choose(c.now(), candidates, explicit)
	if key != "" {
		return key, "", nil
	}
	var why []string
	for key, reason := range reasons {
		why = append(why, key+": "+reason)
	}
	sort.Strings(why)
	if len(why) == 0 {
		why = []string{"no configured machine"}
	}
	return "", strings.Join(why, "; "), nil
}

func reservePlacement(ctx context.Context, tx *store.Tx, thread int64, machine, agent string, now time.Time) error {
	var operation string
	err := tx.QueryRowContext(ctx, `SELECT operation FROM machine_reservations WHERE thread_id=?`, thread).Scan(&operation)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.QueryRowContext(ctx, `SELECT 'placement-'||lower(hex(randomblob(16)))`).Scan(&operation); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return tx.ReserveMachine(ctx, store.MachineReservation{Thread: thread, Machine: machine, Agent: agent, Operation: operation, CreatedAt: now.UnixMilli()})
}

func placementEvent(ctx context.Context, tx *store.Tx, id control.ThreadID, machine, reason string, pending bool, now time.Time) error {
	b, _ := json.Marshal(map[string]any{"machine": machine, "pending": pending, "reason": reason})
	_, err := tx.Append(ctx, control.Event{Thread: id, Time: now, Kind: "placement", Data: b})
	return err
}

// AdmitSession is shared by ordinary launches, wakes and conversation
// replacements. A possibly live reserved effect retains its room while its
// original worker receipts are reconciled; no new reservation can steal it.
func (c *Core) AdmitSession(ctx context.Context, sid int64) error {
	if c.Placement == nil {
		return nil
	}
	var thread int64
	var scope, agent, machine, first, repo string
	var confined bool
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT t.id,t.scope,s.agent,coalesce(t.machine,''),t.confined,s.first_input,t.repo FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE s.id=?`, sid).Scan(&thread, &scope, &agent, &machine, &confined, &first, &repo); err != nil {
		return err
	}
	if confined {
		return nil
	}
	if machine == "" {
		return ErrPlacementWait
	}
	var accepted FirstInput
	if err := json.Unmarshal([]byte(first), &accepted); err != nil {
		return err
	}
	if err := c.recoveryPlacementAuthority(ctx, scope, repo, accepted); err != nil {
		return fmt.Errorf("%w: authorization changed", ErrPlacementWait)
	}
	sc, scopeConfigured := c.Scopes[scope]
	cfg, machineConfigured := c.Placement.Machines[machine]
	if !scopeConfigured || !sc.Enabled || !machineConfigured || sc.Machines != nil && !slices.Contains(sc.Machines, machine) || cfg.Agents != nil && !slices.Contains(cfg.Agents, agent) {
		return ErrPlacementWait
	}
	recovered := false
	if machine == store.LocalMachine {
		if probe, ok := c.Runner.(interface {
			PlacementRecovered(context.Context, int64) (bool, error)
		}); ok {
			var err error
			recovered, err = probe.PlacementRecovered(ctx, sid)
			if err != nil {
				return err
			}
		}
	}
	observations := c.placementObservations(ctx, agent)
	return c.Store.Update(ctx, func(tx *store.Tx) error {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM machine_reservations WHERE thread_id=?`, thread).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 && (machine != store.LocalMachine || recovered) {
			return nil // recovery of this effect owns its original room
		}
		key, why, err := c.choosePlacement(ctx, tx, scope, agent, machine, thread, observations)
		if err != nil {
			return err
		}
		if key == "" {
			return fmt.Errorf("%w: %s", ErrPlacementWait, why)
		}
		return reservePlacement(ctx, tx, thread, key, agent, c.now())
	})
}

// CancelUnplaced closes an accepted start with no machine and no workspace.
// Authorization remains the caller's Close check; this performs no worker I/O.
func (c *Core) cancelUnplaced(ctx context.Context, id control.ThreadID, sid int64, principal control.Principal, reason string) (bool, error) {
	var pending bool
	err := c.Store.Update(ctx, func(tx *store.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_placements p JOIN threads t ON t.id=p.thread_id WHERE t.id=? AND t.machine IS NULL`, int64(id)).Scan(&count); err != nil {
			return err
		}
		pending = count > 0
		if !pending {
			return nil
		}
		if err := c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Close, Principal: principal, Reason: reason}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_placements WHERE thread_id=?`, int64(id)); err != nil {
			return err
		}
		return placementEvent(ctx, tx, id, "", reason, false, c.now())
	})
	return pending, err
}

// DispatchPlacements makes one oldest-first pass. It never makes a workspace
// until the machine choice and reservation have committed together.
func (c *Core) DispatchPlacements(ctx context.Context) error {
	if c.Placement == nil {
		return nil
	}
	defer c.locks.lock("machine-placement")()
	ps, err := c.Store.PendingPlacements(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.dispatchPlacement(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) dispatchPlacement(ctx context.Context, p store.PendingPlacement) error {
	id := control.ThreadID(p.Thread)
	defer c.locks.lock(threadLock(id))()
	var scope, repo, first string
	var l Launch
	var req placementRequest
	if err := json.Unmarshal([]byte(p.Data), &req); err != nil {
		return err
	}
	err := c.Store.DB.QueryRowContext(ctx, `SELECT t.scope,t.repo,s.id,s.agent,s.model,s.effort,s.first_input FROM threads t JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id) WHERE t.id=?`, p.Thread).Scan(&scope, &repo, &l.Session, &l.Agent, &l.Model, &l.Effort, &first)
	if err != nil {
		return err
	}
	f := FirstInput{}
	if err := json.Unmarshal([]byte(first), &f); err != nil {
		return err
	}
	l.AcceptedOperation = f.AcceptedOperation
	l.Thread, l.Principal, l.From, l.Key, l.Input = id, f.Principal, f.From, f.Key, f.Input()
	caller := req.Caller
	var authErr error
	if c.Placement.Caller != nil {
		caller, authErr = c.Placement.Caller(ctx, caller, scope, repo)
	} else if caller.Limit != nil || caller.AsksIn != 0 {
		authErr = ErrForbidden // a fresh daemon grant is required for mediated work
	}
	if authErr == nil {
		authErr = c.As(caller).(*api).act(control.OpStart, "Start", scope)
	}
	sc, configured := c.Scopes[scope]
	_, agentConfigured := c.Agents[l.Agent]
	if c.Agents == nil {
		agentConfigured = l.Agent == c.Agent
	}
	if authErr != nil || !configured || !sc.Enabled || !agentConfigured || sc.Machines != nil && len(sc.Machines) == 0 {
		_, err := c.cancelUnplaced(ctx, id, l.Session, f.Principal, "placement canceled: authorization or scope configuration changed")
		return err
	}
	observations := c.placementObservations(ctx, l.Agent)
	var chosen string
	err = c.Store.Update(ctx, func(tx *store.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_placements WHERE thread_id=?`, p.Thread).Scan(&exists); err != nil || exists == 0 {
			return err
		}
		var why string
		var err error
		chosen, why, err = c.choosePlacement(ctx, tx, scope, l.Agent, p.Machine, p.Thread, observations)
		if err != nil {
			return err
		}
		if chosen == "" {
			_, err = tx.ExecContext(ctx, `UPDATE pending_placements SET reason=? WHERE thread_id=?`, why, p.Thread)
			return err
		}
		if err := reservePlacement(ctx, tx, p.Thread, chosen, l.Agent, c.now()); err != nil {
			return err
		}
		if err := c.Machine.ApplyTx(ctx, tx, l.Session, session.Input{Kind: session.Wake}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET launch=1 WHERE id=?`, l.Session); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_placements WHERE thread_id=?`, p.Thread); err != nil {
			return err
		}
		return placementEvent(ctx, tx, id, chosen, "", false, c.now())
	})
	if err != nil || chosen == "" {
		return err
	}
	return c.launch(ctx, l, repo)
}

// A committed host-start intent may already have spawned its process. Retain
// recovery until the holder reconciles it; an error is not proof of no effect.
func (c *Core) uncertainLocalLaunch(ctx context.Context, sid int64) bool {
	if c.Placement == nil {
		return false
	}
	var phase int
	err := c.Store.DB.QueryRowContext(ctx, `SELECT launch FROM sessions WHERE id=?`, sid).Scan(&phase)
	return err != nil || phase == 2
}
