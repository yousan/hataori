package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// MachineAway supplies observations, never grants. Caller must reconstruct
// current authority; serialized callers carry provenance only.
type MachineAway struct {
	Status func(context.Context, string) (control.MachineAvailability, error)
	Caller func(context.Context, control.Caller, string, string) (control.Caller, error)
}

var ErrActionHeld = control.ErrActionHeld
var ErrMachineUnchecked = errors.New("the owning machine is unavailable or catching up")

type heldAction struct {
	Caller control.Caller `json:"caller"`
	Input  control.Input  `json:"input,omitempty"`
	Answer control.Answer `json:"answer,omitempty"`
	Busy   control.Busy   `json:"busy,omitempty"`
}

// RevalidateInitial runs inside the read commit before unwritten initial
// input. A ready event may have changed creating to idle during catch-up.
func (c *Core) RevalidateInitial(ctx context.Context, tx *store.Tx, sid int64) error {
	if c.Away == nil {
		return nil
	}
	var scope, repo, machine, raw string
	if err := tx.QueryRowContext(ctx, `SELECT t.scope,t.repo,t.machine,s.first_input FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE s.id=?`, sid).Scan(&scope, &repo, &machine, &raw); err != nil {
		return err
	}
	if machine == store.LocalMachine || raw == "" {
		return nil
	}
	var f FirstInput
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return err
	}
	if f.NoInput || f.Written {
		return nil
	}
	old := control.Caller{Principal: f.Principal, From: f.From, Via: f.Via}
	var fresh control.Caller
	var err error
	if c.Away.Caller == nil || !c.Scopes[scope].Enabled {
		err = ErrForbidden
	} else {
		fresh, err = c.Away.Caller(ctx, old, scope, repo)
	}
	op := f.AcceptedOperation
	if op == 0 {
		op = control.OpStart
		if f.Wake {
			op = control.OpSend
		}
	}
	name := "Start"
	if op == control.OpSend {
		name = "Send"
	}
	if err == nil {
		api := c.As(fresh).(*api)
		if api.caller.Principal != old.Principal {
			err = ErrForbidden
		} else {
			err = api.act(op, name, scope)
		}
	}
	if err == nil {
		return nil
	}
	// The host is already owned: withdrawing input must not cancel launch
	// recovery or orphan its follower. Archive provenance before clearing the
	// delivery slot, so a later grant cannot revive this accepted input.
	if _, e := tx.ExecContext(ctx, `INSERT INTO machine_rejected_inputs(session_id,data,reason,created_at) VALUES(?,?,?,?)`, sid, raw, PlacementCanceledReason, time.Now().UnixMilli()); e != nil {
		return e
	}
	if f.Sent && f.TurnID != 0 {
		var current int64
		if e := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM turns WHERE session_id=? AND ended_at IS NULL`, sid).Scan(&current); e != nil {
			return e
		}
		if current == f.TurnID {
			var seq uint64
			if e := tx.QueryRowContext(ctx, `SELECT last_seq+1 FROM sessions WHERE id=?`, sid).Scan(&seq); e != nil {
				return e
			}
			if e := c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.TurnEnded, Seq: seq, Input: "first", Status: agent.Failed, Reason: agent.NotDelivered}}); e != nil {
				return e
			}
		}
	}
	if _, e := tx.ExecContext(ctx, `UPDATE sessions SET first_input='',io_error=?,context=CASE WHEN ?!='' AND set_aside IS NULL THEN ? ELSE context END WHERE id=?`, PlacementCanceledReason, f.Context, f.Context, sid); e != nil {
		return e
	}
	var id control.ThreadID
	if e := tx.QueryRowContext(ctx, `SELECT thread_id FROM sessions WHERE id=?`, sid).Scan(&id); e != nil {
		return e
	}
	data, _ := json.Marshal(map[string]any{"action": "initial input", "state": "refused", "reason": "authorization or scope changed; initial delivery was withdrawn"})
	if _, e := tx.Append(ctx, control.Event{Thread: id, Kind: "machine_action", Time: time.Now(), Data: data}); e != nil {
		return e
	}
	return ErrForbidden
}

func (c *Core) keepPendingAuthority(ctx context.Context, tx *store.Tx, pending int64, caller control.Caller, op control.Ops) error {
	if c.Away == nil {
		return nil
	}
	caller.May = 0
	caller.Scopes = nil
	b, err := json.Marshal(struct {
		Caller    control.Caller
		Operation control.Ops
	}{caller, op})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE pending_inputs SET authority=? WHERE id=?`, string(b), pending)
	return err
}

// RevalidatePending covers accepted-before-disconnection inputs, including the
// race between a successful API readiness check and the first transport write.
func (c *Core) RevalidatePending(ctx context.Context, sid, pending int64) error {
	if c.Away == nil {
		return nil
	}
	var raw, scope, repo, machine string
	var id control.ThreadID
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT p.authority,t.scope,t.repo,t.machine,t.id FROM pending_inputs p JOIN sessions s ON s.id=p.session_id JOIN threads t ON t.id=s.thread_id WHERE p.id=? AND p.session_id=?`, pending, sid).Scan(&raw, &scope, &repo, &machine, &id); err != nil {
		return err
	}
	if machine == store.LocalMachine {
		return nil
	}
	var a struct {
		Caller    control.Caller
		Operation control.Ops
	}
	if raw == "" || json.Unmarshal([]byte(raw), &a) != nil || c.Away.Caller == nil || !c.Scopes[scope].Enabled {
		return ErrForbidden
	}
	fresh, err := c.Away.Caller(ctx, a.Caller, scope, repo)
	if err != nil {
		return err
	}
	api := c.As(fresh).(*api)
	if api.caller.Principal != a.Caller.Principal {
		return ErrForbidden
	}
	name := "Send"
	if a.Operation == control.OpAnswer {
		name = "Answer"
	} else if a.Operation != control.OpSend {
		return ErrForbidden
	}
	if _, err := api.actThread(ctx, a.Operation, name, id); err != nil {
		return err
	}
	return api.ordinaryOnly(ctx, name, scope, id)
}

func (c *Core) machineStatus(ctx context.Context, id control.ThreadID) (string, control.MachineAvailability, error) {
	key, err := c.workspaceMachine(ctx, id)
	if err != nil {
		return key, control.MachineAvailability{}, err
	}
	if c.Away == nil || key == "" || key == store.LocalMachine {
		return key, control.MachineAvailability{State: "ready"}, nil
	}
	a, err := c.Away.Status(ctx, key)
	return key, a, err
}

func (c *Core) requireCheckedMachine(ctx context.Context, id control.ThreadID) error {
	_, a, err := c.machineStatus(ctx, id)
	if err != nil {
		return err
	}
	if a.State != "ready" {
		return fmt.Errorf("%w: %s", ErrMachineUnchecked, a.Reason)
	}
	return nil
}

// hold accepts raw input before prompt hooks. The normal API performs those
// hooks once, after catch-up and a fresh authority check. Completed rows remain
// as idempotency receipts, so repeating an old held key never becomes new input.
func (a *api) hold(ctx context.Context, id control.ThreadID, kind, key, request string, body heldAction) (bool, error) {
	if a.c.Away == nil || a.heldReplay {
		return false, nil
	}
	machine, status, err := a.c.machineStatus(ctx, id)
	if err != nil {
		return false, err
	}
	if machine == "" || machine == store.LocalMachine {
		return false, nil
	}
	if kind == "send" && key == "" {
		key = "unkeyed-" + rand.Text()
	}
	var held bool
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		var receiptState, reason string
		err := tx.QueryRowContext(ctx, `SELECT state,reason FROM machine_held_actions WHERE thread_id=? AND kind=? AND key=?`, int64(id), kind, key).Scan(&receiptState, &reason)
		if err == nil {
			held = true
			if receiptState == "refused" {
				return fmt.Errorf("held action refused: %s: %w", reason, session.ErrRefused)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM machine_held_actions WHERE thread_id=? AND state IN ('pending','dispatching')`, int64(id)).Scan(&pending); err != nil {
			return err
		}
		if status.State == "ready" && pending == 0 {
			return nil
		}
		sid, state, err := a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		if state == control.Closed || state == control.Removed || state == control.Failed {
			return ErrClosed
		}
		var run string
		if err := tx.QueryRowContext(ctx, `SELECT run FROM session_machine_runs WHERE session_id=?`, sid).Scan(&run); err != nil {
			return err
		}
		var turn int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM turns WHERE session_id=? AND ended_at IS NULL`, sid).Scan(&turn); err != nil {
			return err
		}
		if kind == "stop" {
			if request != "" && request != fmt.Sprint(turn) {
				return session.ErrRefused
			}
			if turn == 0 {
				return session.ErrRefused
			}
			key = fmt.Sprintf("%s:%s:%d", a.caller.Principal, run, turn)
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM machine_held_actions WHERE thread_id=? AND kind='stop' AND key=?`, int64(id), key).Scan(&exists); err != nil {
				return err
			}
			if exists != 0 {
				held = true
				return nil
			}
			request = ""
		}
		if kind == "send" && state == control.Waiting && !a.caller.NoAnswer {
			if err := tx.QueryRowContext(ctx, `SELECT handle FROM requests WHERE session_id=? AND state='open' ORDER BY id DESC LIMIT 1`, sid).Scan(&request); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		body.Caller = a.caller
		body.Caller.May = 0
		body.Caller.Scopes = nil
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO machine_held_actions(thread_id,session_id,run,turn,request,kind,key,data,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, int64(id), sid, run, turn, request, kind, key, string(data), a.c.now().UnixMilli()); err != nil {
			return err
		}
		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM machine_availability WHERE machine=?`, machine).Scan(&epoch); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO machine_absence_notices(machine,thread_id,epoch) VALUES(?,?,?)`, machine, int64(id), epoch)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 0 {
			data, _ := json.Marshal(map[string]any{"action": "held", "state": "pending", "reason": "The machine is unavailable; accepted actions wait for reconciliation. No pending stop has stopped anything."})
			if _, err := tx.Append(ctx, control.Event{Thread: id, Kind: "machine_action", Time: a.c.now(), Data: data}); err != nil {
				return err
			}
		}
		held = true
		return nil
	})
	return held, err
}

// DispatchHeld follows durable per-thread order. It re-enters the same API,
// using rebuilt grants and the original effect key. A crash at an uncertain
// stop boundary is reported rather than blindly interrupting another turn.
func (c *Core) DispatchHeld(ctx context.Context) error {
	if c.Away == nil {
		return nil
	}
	defer c.locks.lock("machine-held-dispatch")()
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT id,thread_id,session_id,run,turn,request,kind,key,data,state FROM machine_held_actions WHERE state IN ('pending','dispatching') ORDER BY id`)
	if err != nil {
		return err
	}
	type item struct {
		id, thread, sid, turn                int64
		run, request, kind, key, data, state string
	}
	var items []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.thread, &v.sid, &v.run, &v.turn, &v.request, &v.kind, &v.key, &v.data, &v.state); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	blocked := map[int64]bool{}
	for _, v := range items {
		if blocked[v.thread] {
			continue
		}
		id := control.ThreadID(v.thread)
		if err := c.requireCheckedMachine(ctx, id); err != nil {
			blocked[v.thread] = true
			continue
		}
		var body heldAction
		if err = json.Unmarshal([]byte(v.data), &body); err != nil {
			return err
		}
		var scope, repo, run string
		var sid int64
		if err = c.Store.DB.QueryRowContext(ctx, `SELECT t.scope,t.repo,s.id,x.run FROM threads t JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id) JOIN session_machine_runs x ON x.session_id=s.id WHERE t.id=?`, v.thread).Scan(&scope, &repo, &sid, &run); err != nil {
			return err
		}
		var cause error
		if !c.Scopes[scope].Enabled {
			cause = ErrDisabled
		}
		if c.Away.Caller == nil {
			cause = ErrForbidden
		}
		var fresh control.Caller
		if cause == nil {
			fresh, cause = c.Away.Caller(ctx, body.Caller, scope, repo)
		}
		if cause == nil && c.As(fresh).(*api).caller.Principal != body.Caller.Principal {
			cause = ErrForbidden
		}
		if cause == nil && (sid != v.sid || run != v.run) {
			cause = errors.New("the original session or host run has ended")
		}
		if cause == nil && v.request != "" {
			var n int
			cause = c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE session_id=? AND handle=? AND state='open'`, v.sid, v.request).Scan(&n)
			if cause == nil && n != 1 {
				cause = ErrNotOpen
			}
		}
		if cause == nil && v.kind == "stop" {
			var n int
			cause = c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM turns WHERE id=? AND session_id=? AND ended_at IS NULL`, v.turn, v.sid).Scan(&n)
			if cause == nil && n != 1 {
				cause = errors.New("the original turn has ended; the pending stop is stale")
			}
			if v.state == "dispatching" {
				cause = errors.New("the pending stop's delivery is uncertain; it was not retried")
			}
		}
		if cause == nil {
			if _, err := c.Store.DB.ExecContext(ctx, `UPDATE machine_held_actions SET state='dispatching' WHERE id=?`, v.id); err != nil {
				return err
			}
			a := c.As(fresh).(*api)
			a.heldReplay = true
			a.heldActionID = v.id
			switch v.kind {
			case "send":
				if v.request == "" {
					a.caller.NoAnswer = true
				}
				busy := body.Busy
				if busy == control.Interrupt {
					busy = control.Refuse
				}
				cause = a.send(ctx, id, v.key, body.Input, busy, &sendGuard{held: true, proposal: true, session: v.sid, run: v.run, question: v.request})
			case "answer":
				cause = a.Answer(ctx, control.RequestID(v.request), body.Answer)
			case "stop":
				cause = a.Stop(ctx, id, v.turn)
			default:
				cause = errors.New("unknown held action")
			}
		}
		// Acceptance and its durable held receipt commit together before a
		// worker write. A transport error afterwards cannot replay the API or
		// mislabel an accepted answer as a refusal after its request closed.
		var receipt string
		if err := c.Store.DB.QueryRowContext(ctx, `SELECT state FROM machine_held_actions WHERE id=?`, v.id).Scan(&receipt); err != nil {
			return err
		}
		if receipt == "dispatched" {
			continue
		}
		if errors.Is(cause, ErrBusy) || errors.Is(cause, ErrMachineUnchecked) {
			if _, err := c.Store.DB.ExecContext(ctx, `UPDATE machine_held_actions SET state='pending' WHERE id=?`, v.id); err != nil {
				return err
			}
			blocked[v.thread] = true
			continue
		}
		state, reason := "dispatched", ""
		if cause != nil {
			state, reason = "refused", heldRefusalReason(cause)
		}
		if err := c.Store.Update(ctx, func(tx *store.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE machine_held_actions SET state=?,reason=? WHERE id=?`, state, reason, v.id); err != nil {
				return err
			}
			data, _ := json.Marshal(map[string]any{"action": v.kind, "state": state, "reason": reason})
			_, err := tx.Append(ctx, control.Event{Thread: id, Kind: "machine_action", Time: time.Now(), Data: data})
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// acceptHeld commits the normal API acceptance and absence-queue receipt in
// the same transaction. "dispatched" means handed to the durable input queue;
// agent delivery continues to use the original pending input/effect receipt.
func (a *api) acceptHeld(ctx context.Context, tx *store.Tx) error {
	if a.heldActionID == 0 {
		return nil
	}
	var id control.ThreadID
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id,kind FROM machine_held_actions WHERE id=?`, a.heldActionID).Scan(&id, &kind); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE machine_held_actions SET state='dispatched',reason='' WHERE id=?`, a.heldActionID); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"action": kind, "state": "dispatched", "reason": ""})
	_, err := tx.Append(ctx, control.Event{Thread: id, Kind: "machine_action", Time: time.Now(), Data: data})
	return err
}

func heldRefusalReason(err error) string {
	switch {
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrDisabled):
		return "current person, scope or operation no longer authorizes this action"
	case errors.Is(err, ErrNotOpen):
		return "the original request is no longer open"
	case errors.Is(err, errGuardEnded), errors.Is(err, session.ErrRefused):
		return "the original session, run, turn or request changed"
	default:
		return "the queued action could not safely target its original operation; it was not retried"
	}
}
