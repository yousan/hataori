package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

const DeferredKeyPrefix = "deferred:"

// DeferredInputs reconstructs grants from trusted runtime metadata. Neither
// its policy nor its transport is supplied by an API request or saved caller.
type DeferredInputs struct {
	Core      *Core
	Policy    func(scope string, issuer control.ThreadID) (control.Caller, []string, error)
	Transport func(context.Context, int64) (string, error)
	mu        sync.Mutex
	wake      chan struct{}
}

type deferredPayload struct {
	SourceSession                                 int64
	Principal                                     control.Principal
	From, Mark, Text, Repo, Machine, Conversation string
	Workspace                                     int64
	QuestionPos                                   uint64
	ApprovedBy                                    control.Principal
	ApprovedFrom                                  string
}

// DeferredBatch is a frozen emitted action. Command is adapter-owned opaque
// correlation evidence; Data is the exact encoded receiver write.
type DeferredBatch struct {
	Number         int64 // durable codec identity, independent of reusable delivery slots
	Run, Key       string
	Data, Command  [][]byte
	Digest, Result string
	CompletedAt    int64
}

type deferredRecord struct {
	control.DeferredSend
	scope, key, input string
	issuer            control.ThreadID
	payload           deferredPayload
	batch             DeferredBatch
}

type deferredReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readDeferred(ctx context.Context, q deferredReader, where string, args ...any) (r deferredRecord, err error) {
	var raw, batch string
	var created, updated int64
	err = q.QueryRowContext(ctx, `SELECT id,scope,issuer,key,target,session,state,reason,data,proposal,input_ref,handoff,created_at,updated_at
		FROM deferred_sends WHERE `+where, args...).Scan(&r.Number, &r.scope, &r.issuer, &r.key, &r.Target, &r.Session, &r.State, &r.Reason, &raw, &r.Proposal, &r.input, &batch, &created, &updated)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal([]byte(raw), &r.payload); err != nil {
		return r, err
	}
	if batch != "" {
		if err = json.Unmarshal([]byte(batch), &r.batch); err != nil {
			return r, err
		}
	}
	r.Text = r.payload.Text
	r.Created, r.Updated = time.UnixMilli(created), time.UnixMilli(updated)
	return r, nil
}

func (a *api) DeferredSend(ctx context.Context, req control.DeferredSendRequest) (control.DeferredSend, error) {
	if a.c.Deferred == nil {
		a.c.Log.Warn("control call refused", "op", "DeferredSend", "principal", string(a.caller.Principal), "reason", "durable deferred input is unavailable")
		return control.DeferredSend{}, control.ErrNotSupported
	}
	return a.c.Deferred.call(ctx, a, req)
}

func (d *DeferredInputs) visible(a *api, r deferredRecord) error {
	if !a.sees(r.scope) {
		return ErrNotFound
	}
	if err := a.check(control.OpRead, "DeferredSend", r.scope); err != nil {
		return err
	}
	if a.caller.AsksIn != 0 && a.caller.AsksIn != r.issuer {
		return ErrNotFound
	}
	if a.caller.AsksIn == 0 && a.caller.Principal != control.Principal(a.c.People.Owner()) {
		return ErrForbidden
	}
	return nil
}

func (d *DeferredInputs) answer(a *api, r deferredRecord) control.DeferredSend {
	ans := r.DeferredSend
	if a.caller.NoText {
		ans.Text = ""
	}
	return ans
}

func (d *DeferredInputs) find(ctx context.Context, a *api, req control.DeferredSendRequest) (deferredRecord, error) {
	if a.caller.AsksIn == 0 || req.Key == "" {
		return deferredRecord{}, ErrForbidden
	}
	r, err := readDeferred(ctx, d.Core.Store.DB, "issuer=? AND key=?", a.caller.AsksIn, req.Key)
	if err != nil {
		return r, err
	}
	if err = d.visible(a, r); err != nil {
		return r, err
	}
	if r.Target != req.Target || r.payload.Text != req.Text {
		return r, fmt.Errorf("deferred retry key names a different instruction: %w", session.ErrRefused)
	}
	return r, nil
}

func (d *DeferredInputs) call(ctx context.Context, a *api, req control.DeferredSendRequest) (control.DeferredSend, error) {
	if req.Action == "queue" || req.Action == "find" {
		if req.Key == "" || len(req.Key) > 128 || strings.TrimSpace(req.Text) == "" || len(req.Text) > 16*1024 || req.Target <= 0 {
			return control.DeferredSend{}, fmt.Errorf("deferred input needs target, text (at most 16 KiB) and retry key (at most 128 bytes): %w", session.ErrRefused)
		}
		r, err := d.find(ctx, a, req)
		if err == nil {
			return d.answer(a, r), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return control.DeferredSend{}, err
		}
		if req.Action == "find" {
			return control.DeferredSend{State: "absent"}, nil
		}
		return d.enqueue(ctx, a, req)
	}
	if req.Action != "inspect" && req.Action != "cancel" {
		return control.DeferredSend{}, fmt.Errorf("unknown deferred action: %w", session.ErrRefused)
	}
	r, err := readDeferred(ctx, d.Core.Store.DB, "id=?", req.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return control.DeferredSend{}, ErrNotFound
	}
	if err != nil {
		return control.DeferredSend{}, err
	}
	if err = d.visible(a, r); err != nil {
		return control.DeferredSend{}, err
	}
	if req.Action == "cancel" {
		err = d.Core.Store.Update(ctx, func(tx *store.Tx) error {
			var err error
			r, err = readDeferred(ctx, tx, "id=?", r.Number)
			if err != nil {
				return err
			}
			if r.State == "proposed" || r.State == "queued" || r.State == "accepted" {
				if err = d.transitionTx(ctx, tx, &r, "cancelled", "cancelled before the frozen handoff"); err != nil {
					return err
				}
				if r.Proposal != 0 {
					_, err = tx.ExecContext(ctx, `UPDATE proposals SET state='cancelled',closed_at=? WHERE id=? AND state IN ('open','approved')`, d.Core.now().UnixMilli(), r.Proposal)
				}
			}
			return err
		})
		if err != nil {
			return control.DeferredSend{}, err
		}
		if r.State == "handoff" || r.State == "delivered" {
			r.Reason = "too late to cancel: the frozen handoff may already have begun"
		}
	}
	return d.answer(a, r), nil
}

func (d *DeferredInputs) enqueue(ctx context.Context, a *api, req control.DeferredSendRequest) (control.DeferredSend, error) {
	if !a.caller.DeferInput || a.caller.AsksIn == 0 {
		return control.DeferredSend{}, a.refused("DeferredSend", "", "the caller cannot defer input")
	}
	scope, err := a.actThread(ctx, control.OpSend, "DeferredSend", req.Target)
	if err != nil {
		return control.DeferredSend{}, err
	}
	if err = a.ordinaryOnly(ctx, "DeferredSend", scope, req.Target); err != nil {
		return control.DeferredSend{}, err
	}
	if d.Policy == nil || d.Transport == nil {
		return control.DeferredSend{}, control.ErrNotSupported
	}
	var sid int64
	var state control.State
	err = d.Core.Store.Update(ctx, func(tx *store.Tx) error { var e error; sid, state, e = d.Core.current(ctx, tx, req.Target); return e })
	if err != nil {
		return control.DeferredSend{}, err
	}
	if state != control.Running && state != control.Idle {
		return control.DeferredSend{}, fmt.Errorf("deferred acceptance needs an existing running or idle worker: %w", session.ErrRefused)
	}
	// Unsupported holders/adapters/old hosts refuse before durable artifacts.
	if run, e := d.Transport(ctx, sid); e != nil || run == "" {
		return control.DeferredSend{}, fmt.Errorf("deferred input unsupported by this worker transport: %w", control.ErrNotSupported)
	}
	var r deferredRecord
	err = d.Core.Store.Update(ctx, func(tx *store.Tx) error {
		if old, e := readDeferred(ctx, tx, "issuer=? AND key=?", a.caller.AsksIn, req.Key); e == nil {
			if old.Target != req.Target || old.payload.Text != req.Text {
				return session.ErrRefused
			}
			r = old
			return nil
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		r = deferredRecord{DeferredSend: control.DeferredSend{Target: req.Target, Session: sid, State: "queued"}, scope: scope, issuer: a.caller.AsksIn, key: req.Key}
		r.payload = deferredPayload{Principal: a.caller.Principal, From: a.caller.From, Mark: a.caller.Mark, Text: req.Text}
		var sourceState string
		if err := tx.QueryRowContext(ctx, `SELECT id,state FROM sessions WHERE thread_id=? ORDER BY id DESC LIMIT 1`, r.issuer).Scan(&r.payload.SourceSession, &sourceState); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT t.repo,t.machine,w.id,s.agent_session_id FROM threads t JOIN workspaces w ON w.thread_id=t.id JOIN sessions s ON s.id=? WHERE t.id=?`, sid, r.Target).Scan(&r.payload.Repo, &r.payload.Machine, &r.payload.Workspace, &r.payload.Conversation); err != nil {
			return err
		}
		if r.payload.Conversation == "" {
			return fmt.Errorf("worker conversation identity is not established: %w", session.ErrRefused)
		}
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(pos),0) FROM events WHERE thread_id=? AND kind='request'`, r.Target).Scan(&r.payload.QuestionPos); err != nil {
			return err
		}
		fresh, err := d.authorityTx(ctx, tx, r, false)
		if err != nil {
			return err
		}
		if state, e := d.targetTx(ctx, tx, r); e != nil {
			return e
		} else if state != control.Running && state != control.Idle {
			return ErrBusy
		}
		if fresh.Propose || a.caller.Propose {
			r.State = "proposed"
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deferred_sends WHERE target=? AND state IN ('proposed','queued','accepted','handoff')`, r.Target).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("the worker already has a pending deferred instruction: %w", ErrBusy)
		}
		data, _ := json.Marshal(r.payload)
		now := d.Core.now()
		if err := tx.QueryRowContext(ctx, `INSERT INTO deferred_sends(scope,issuer,key,target,session,state,data,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, scope, r.issuer, r.key, r.Target, r.Session, r.State, string(data), now.UnixMilli(), now.UnixMilli()).Scan(&r.Number); err != nil {
			return err
		}
		r.Text, r.Created, r.Updated = req.Text, now, now
		if r.State == "proposed" {
			return d.proposeTx(ctx, tx, &r)
		}
		return nil
	})
	if err != nil {
		return control.DeferredSend{}, err
	}
	d.signal()
	return d.answer(a, r), nil
}

func (d *DeferredInputs) authorityTx(ctx context.Context, tx *store.Tx, r deferredRecord, delivery bool) (control.Caller, error) {
	refuse := func() (control.Caller, error) {
		return control.Caller{}, fmt.Errorf("deferred caller, scope, repository or approval authority changed: %w", ErrForbidden)
	}
	if d.Policy == nil || !d.Core.Scopes[r.scope].Enabled {
		return refuse()
	}
	c, repos, err := d.Policy(r.scope, r.issuer)
	if err != nil || !c.DeferInput || c.From != r.payload.From || !slices.Contains(repos, r.payload.Repo) {
		return refuse()
	}
	a := d.Core.As(c).(*api)
	if a.caller.Principal != r.payload.Principal || a.act(control.OpSend, "DeferredSend", r.scope) != nil {
		return refuse()
	}
	var sid int64
	var state, scope string
	var confined bool
	if err = tx.QueryRowContext(ctx, `SELECT s.id,s.state,t.scope,t.confined FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE t.id=? ORDER BY s.id DESC LIMIT 1`, r.issuer).Scan(&sid, &state, &scope, &confined); err != nil {
		return refuse()
	}
	if sid != r.payload.SourceSession || scope != r.scope || !confined || state == string(control.Closed) || state == string(control.Removed) || state == string(control.Failed) {
		return refuse()
	}
	if delivery && c.Propose && r.payload.ApprovedBy == "" {
		return refuse()
	}
	if r.payload.ApprovedBy != "" {
		if r.payload.ApprovedBy == r.payload.Principal || !d.Core.People.May(string(r.payload.ApprovedBy), r.scope) {
			return refuse()
		}
		if r.payload.ApprovedFrom != "" {
			name, _ := d.Core.People.Resolve(r.payload.ApprovedFrom)
			if name != string(r.payload.ApprovedBy) {
				return refuse()
			}
		}
	}
	return a.caller, nil
}

func (d *DeferredInputs) targetTx(ctx context.Context, tx *store.Tx, r deferredRecord) (control.State, error) {
	var sid, workspace int64
	var state control.State
	var conversation, repo, machine, scope string
	var confined bool
	err := tx.QueryRowContext(ctx, `SELECT s.id,s.state,s.agent_session_id,w.id,t.repo,t.machine,t.scope,t.confined FROM threads t JOIN sessions s ON s.thread_id=t.id JOIN workspaces w ON w.thread_id=t.id WHERE t.id=? ORDER BY s.id DESC LIMIT 1`, r.Target).Scan(&sid, &state, &conversation, &workspace, &repo, &machine, &scope, &confined)
	if err != nil {
		return state, err
	}
	if sid != r.Session || conversation != r.payload.Conversation || workspace != r.payload.Workspace || repo != r.payload.Repo || machine != r.payload.Machine || machine != store.LocalMachine || scope != r.scope || confined {
		return state, fmt.Errorf("deferred worker identity changed: %w", session.ErrRefused)
	}
	if state == control.Closed || state == control.Removed || state == control.Failed {
		return state, ErrClosed
	}
	var questions int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM requests WHERE session_id=? AND state='open') + (SELECT count(*) FROM events WHERE thread_id=? AND kind='request' AND pos>?)`, sid, r.Target, r.payload.QuestionPos).Scan(&questions); err != nil {
		return state, err
	}
	if questions != 0 || state == control.Waiting {
		return state, fmt.Errorf("a question invalidated the deferred instruction: %w", session.ErrRefused)
	}
	return state, nil
}

func (d *DeferredInputs) transitionTx(ctx context.Context, tx *store.Tx, r *deferredRecord, state, reason string) error {
	if r.State == "proposed" && (state == "refused" || state == "failed" || state == "cancelled") && r.Proposal != 0 {
		if _, e := tx.ExecContext(ctx, `UPDATE proposals SET state='cancelled',closed_at=? WHERE id=? AND state='open'`, d.Core.now().UnixMilli(), r.Proposal); e != nil {
			return e
		}
	}
	now := d.Core.now()
	_, err := tx.ExecContext(ctx, `UPDATE deferred_sends SET state=?,reason=?,updated_at=? WHERE id=?`, state, reason, now.UnixMilli(), r.Number)
	if err == nil {
		r.State, r.Reason, r.Updated = state, reason, now
	}
	return err
}

func (d *DeferredInputs) signal() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wake != nil {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
}

func (d *DeferredInputs) Run(ctx context.Context) {
	d.mu.Lock()
	d.wake = make(chan struct{}, 1)
	wake := d.wake
	d.mu.Unlock()
	var pos uint64
	d.Core.Store.DB.QueryRowContext(ctx, `SELECT coalesce(max(pos),0) FROM events`).Scan(&pos)
	events := make(chan control.Event, 1)
	go func() { defer close(events); d.Core.Store.Follow(ctx, control.Filter{}, pos, events) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	d.DispatchOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-wake:
		case <-tick.C:
		}
		d.DispatchOnce(ctx)
	}
}

func (d *DeferredInputs) DispatchOnce(ctx context.Context) {
	rows, err := d.Core.Store.DB.QueryContext(ctx, `SELECT id FROM deferred_sends WHERE state IN ('queued','proposed','accepted') ORDER BY id`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var n int64
		if rows.Scan(&n) == nil {
			ids = append(ids, n)
		}
	}
	rows.Close()
	for _, n := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := d.dispatch(ctx, n); err != nil {
			d.Core.Log.Warn("deferred input waits", "number", n, "err", err)
		}
	}
}

func (d *DeferredInputs) dispatch(ctx context.Context, n int64) error {
	var r deferredRecord
	var caller control.Caller
	var state control.State
	err := d.Core.Store.Update(ctx, func(tx *store.Tx) error {
		var e error
		r, e = readDeferred(ctx, tx, "id=?", n)
		if e != nil {
			return e
		}
		if r.State != "queued" && r.State != "proposed" && r.State != "accepted" {
			return nil
		}
		caller, e = d.authorityTx(ctx, tx, r, r.State != "proposed")
		if e == nil {
			state, e = d.targetTx(ctx, tx, r)
		}
		if e != nil {
			return d.transitionTx(ctx, tx, &r, "refused", e.Error())
		}
		return nil
	})
	if err != nil || r.State != "queued" {
		return err
	}
	if state != control.Idle && state != control.Asleep {
		return nil
	}
	caller.Propose = false
	caller.NoAnswer = true
	caller.RefuseBusy = true
	caller.OrdinaryOnly = true
	caller.Via = control.ViaDeferred
	a := d.Core.As(caller).(*api)
	a.deferredID = r.Number
	err = a.send(ctx, r.Target, DeferredKeyPrefix+strconv.FormatInt(r.Number, 10), control.Input{Text: r.payload.Text}, control.Refuse, &sendGuard{deferred: r.Number})
	if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrMachineUnchecked) {
		return nil
	}
	return d.Core.Store.Update(ctx, func(tx *store.Tx) error {
		now, e := readDeferred(ctx, tx, "id=?", n)
		if e != nil {
			return e
		}
		if now.State == "queued" {
			return d.transitionTx(ctx, tx, &now, "refused", err.Error())
		}
		return nil // input acceptance committed; its normal recovery owns it
	})
}

func (d *DeferredInputs) guardTx(ctx context.Context, tx *store.Tx, n int64, id control.ThreadID) error {
	r, err := readDeferred(ctx, tx, "id=?", n)
	if err != nil {
		return err
	}
	if r.State != "queued" || r.Target != id {
		return session.ErrRefused
	}
	if _, err = d.authorityTx(ctx, tx, r, true); err != nil {
		return err
	}
	state, err := d.targetTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if state != control.Idle && state != control.Asleep {
		return ErrBusy
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pending_inputs WHERE session_id=?) + (SELECT count(*) FROM pending_placements WHERE thread_id=?) + (SELECT count(*) FROM machine_held_actions WHERE thread_id=? AND state IN ('pending','dispatching')) + (SELECT count(*) FROM sessions WHERE id=? AND (first_input!='' OR set_aside IS NOT NULL)) + (SELECT count(*) FROM turns WHERE session_id=? AND ended_at IS NULL)`, r.Session, r.Target, r.Target, r.Session, r.Session).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return ErrBusy
	}
	return nil
}

func (d *DeferredInputs) acceptedTx(ctx context.Context, tx *store.Tx, n, sid, pending int64, wake bool) error {
	r, err := readDeferred(ctx, tx, "id=?", n)
	if err != nil {
		return err
	}
	if r.State != "queued" || r.Session != sid {
		return session.ErrRefused
	}
	input := strconv.FormatInt(pending, 10)
	if wake {
		input = "first"
	}
	if input == "0" {
		return session.ErrRefused
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deferred_sends SET input_ref=? WHERE id=?`, input, n); err != nil {
		return err
	}
	return d.transitionTx(ctx, tx, &r, "accepted", "")
}
