package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Connection is a verified transport generation. Connected allows recovery
// reads only; durable catch-up acknowledgments separately authorize dispatch.
type Connection struct {
	Connected     bool
	Token, Reason string
	Immediate     bool
}

func (r *Runner) connection(key string) Connection {
	m, ok := r.Remotes[key]
	if !ok || m.Connection == nil {
		return Connection{Reason: "owning machine is not configured or registered", Immediate: true}
	}
	return m.Connection()
}

func (r *Runner) availability(ctx context.Context, key string) (store.Availability, error) {
	c := r.connection(key)
	grace := r.AwayGrace
	if grace == 0 {
		grace = 30 * time.Second
	}
	a, err := r.Store.ObserveConnection(ctx, key, c.Connected, c.Token, c.Reason, c.Immediate, grace, time.Now())
	if err != nil {
		return a, err
	}
	if c.Connected {
		if err := r.Store.FinishCatchup(ctx, key, c.Token, time.Now()); err != nil {
			return a, err
		}
		return r.Store.Availability(ctx, key)
	}
	return a, nil
}

func (r *Runner) MachineAvailability(ctx context.Context, key string) (control.MachineAvailability, error) {
	if key == store.LocalMachine {
		return control.MachineAvailability{State: "ready", At: time.Now()}, nil
	}
	a, err := r.availability(ctx, key)
	at := a.ObservedAt
	if a.FirstFailure != 0 {
		at = a.FirstFailure
	}
	return control.MachineAvailability{State: a.State, At: time.UnixMilli(at), Reason: a.Reason}, err
}

func (r *Runner) sessionMachine(id int64) (string, error) {
	var key string
	err := r.Store.DB.QueryRow(`SELECT t.machine FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE s.id=?`, id).Scan(&key)
	return key, err
}

func (r *Runner) sessionChecked(id int64) bool {
	if !r.AwayEnabled {
		return true
	}
	key, err := r.sessionMachine(id)
	if err != nil {
		return false
	}
	if key == store.LocalMachine {
		return true
	}
	a, err := r.availability(context.Background(), key)
	return err == nil && a.State == "ready"
}

func (r *Runner) observeMachines(ctx context.Context) error {
	if !r.AwayEnabled {
		return nil
	}
	ms, err := r.Store.Machines(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Key != store.LocalMachine {
			if _, err := r.availability(ctx, m.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// beginCatchup blocks immediately on a disconnected transport. Reads during
// catch-up reconstruct the codec and original receipt journal, with no new
// input, idle timeout or codec Tick allowed to precede the retained output.
func (r *Runner) beginCatchup(lv *live) bool {
	if !r.AwayEnabled || !lv.remote {
		return true
	}
	key, err := r.sessionMachine(lv.id)
	if err != nil {
		r.pauseIO(lv, err)
		return false
	}
	a, err := r.availability(context.Background(), key)
	if err != nil {
		r.pauseIO(lv, err)
		return false
	}
	if a.State != "ready" {
		lv.catchingUp = true
		lv.catchupToken = a.Token
		if !r.connection(key).Connected {
			lv.warmPending = true
			r.pauseIO(lv, core.ErrMachineUnchecked)
			return false
		}
	}
	return true
}

func (r *Runner) finishSessionCatchup(lv *live) bool {
	if !r.AwayEnabled || !lv.remote {
		return true
	}
	key, err := r.sessionMachine(lv.id)
	if err != nil {
		return false
	}
	if lv.catchingUp {
		if reconciler, ok := lv.codec.(agent.HookReconciler); ok {
			waiting, err := lv.io.Waiting(context.Background())
			if err != nil {
				r.pauseIO(lv, err)
				return false
			}
			ids := make([]string, 0, len(waiting))
			for _, w := range waiting {
				ids = append(ids, w.ID)
			}
			if err := r.do(lv, reconciler.ReconcileHooks(ids)); err != nil {
				r.pauseIO(lv, err)
				return false
			}
		}
		if err := r.Store.AcknowledgeCatchup(context.Background(), lv.id, lv.catchupToken); err != nil {
			return false
		}
	}
	a, err := r.availability(context.Background(), key)
	if err != nil || a.State != "ready" {
		return false
	}
	lv.catchingUp = false
	return true
}

func (r *Runner) noteRemoteReadFailure(lv *live) {
	if !r.AwayEnabled || !lv.remote {
		return
	}
	key, err := r.sessionMachine(lv.id)
	if err != nil {
		return
	}
	c := r.connection(key)
	if m := r.Remotes[key]; m.Failed != nil {
		m.Failed(lv.readErr)
	}
	c = r.connection(key)
	r.Store.ObserveConnection(context.Background(), key, false, c.Token, "retained worker I/O is unavailable", c.Immediate, r.awayGrace(), time.Now())
	lv.catchingUp = true
	lv.warmPending = true
}

func (r *Runner) awayGrace() time.Duration {
	if r.AwayGrace > 0 {
		return r.AwayGrace
	}
	return 30 * time.Second
}

func (r *Runner) activeAge(ctx context.Context, q store.RowQuerier, id int64, since time.Time) time.Duration {
	if !r.AwayEnabled {
		return time.Since(since)
	}
	age, err := store.ActiveAge(ctx, q, id, since, time.Now())
	if err != nil {
		return 0
	}
	return age
}

func (r *Runner) codecNow(lv *live) time.Time {
	if !r.AwayEnabled || !lv.remote {
		return time.Now()
	}
	now := time.Now()
	since := time.Unix(0, 0)
	age, err := store.ActiveAge(context.Background(), r.Store.DB, lv.id, since, now)
	if err != nil {
		return since
	}
	return since.Add(age)
}

// Only a definite verified inventory entry can replace unavailable retained
// output with interrupted recovery. An unknown/missing entry is never proof.
func (r *Runner) confirmedGone(lv *live) bool {
	if !r.AwayEnabled || !lv.remote {
		return false
	}
	key, err := r.sessionMachine(lv.id)
	if err != nil || !r.connection(key).Connected {
		return false
	}
	var state string
	err = r.Store.DB.QueryRow(`SELECT i.state FROM machine_inventory i JOIN session_machine_runs x ON x.machine=i.machine AND x.run=i.run AND i.session='s-'||x.session_id WHERE x.session_id=?`, lv.id).Scan(&state)
	return err == nil && state == "gone"
}

func (r *Runner) dispatchAllowed(lv *live) error {
	if !r.sessionChecked(lv.id) || lv.catchingUp || lv.warmPending || lv.readErr != nil {
		return core.ErrMachineUnchecked
	}
	if lv.ended {
		return errors.New("session ended")
	}
	return nil
}

// writtenClock translates a durable wall timestamp into the codec's active
// clock without resetting the input's delivery budget on reconstruction.
func (r *Runner) writtenClock(lv *live, at int64) time.Time {
	if at == 0 {
		return time.Time{}
	}
	written := time.UnixMilli(at)
	if !r.AwayEnabled || !lv.remote {
		return written
	}
	elapsed, err := store.ActiveAge(context.Background(), r.Store.DB, lv.id, written, time.Now())
	if err != nil {
		return r.codecNow(lv)
	}
	return r.codecNow(lv).Add(-elapsed)
}

func (r *Runner) tickCodec(lv *live) agent.Actions {
	if lv.deferredBlocked {
		return agent.Actions{}
	}
	if c, ok := lv.codec.(agent.ClockedTicker); ok {
		return c.TickClocks(r.codecNow(lv), time.Now())
	}
	return lv.codec.Tick(r.codecNow(lv))
}

func (r *Runner) actionInputs(lv *live, inputs []string) ([]string, error) {
	out := append([]string{}, inputs...)
	if lv.answerPending != 0 {
		var key string
		if err := r.Store.DB.QueryRow(`SELECT receipt_key FROM pending_inputs WHERE id=? AND session_id=?`, lv.answerPending, lv.id).Scan(&key); err != nil {
			return nil, err
		}
		if key == "" {
			return nil, errors.New("runner: pending answer has no receipt identity")
		}
		out = append(out, "answer:"+strconv.FormatInt(lv.answerPending, 10)+":"+key)
	}
	return out, nil
}

// refusePendingAnswer records a terminal codec/receipt refusal before retiring
// the accepted answer; it never retries API acceptance or answers another hook.
func (r *Runner) refusePendingAnswer(sid, pending int64) error {
	ctx := context.Background()
	return r.Store.Update(ctx, func(tx *store.Tx) error { return r.refusePendingAnswerTx(ctx, tx, sid, pending) })
}

func (r *Runner) refusePendingAnswerTx(ctx context.Context, tx *store.Tx, sid, pending int64, reasons ...string) error {
	var held, turn int64
	var id control.ThreadID
	err := tx.QueryRowContext(ctx, `SELECT p.held_action,p.caller_turn,s.thread_id FROM pending_inputs p JOIN sessions s ON s.id=p.session_id WHERE p.id=? AND p.session_id=?`, pending, sid).Scan(&held, &turn, &id)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if turn != 0 {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM turns WHERE session_id=? AND ended_at IS NULL`, sid).Scan(&current); err != nil {
			return err
		}
		if current == turn {
			var seq uint64
			if err := tx.QueryRowContext(ctx, `SELECT last_seq+1 FROM sessions WHERE id=?`, sid).Scan(&seq); err != nil {
				return err
			}
			if err := r.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.TurnEnded, Seq: seq, Status: agent.Failed, Reason: agent.NotDelivered}}); err != nil {
				return err
			}
		}
	}
	reason := "the original request no longer accepts this answer; no replacement request was answered"
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	if held != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE machine_held_actions SET state='refused',reason=? WHERE id=?`, reason, held); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(map[string]any{"action": "answer", "state": "refused", "reason": reason})
	if _, err := tx.Append(ctx, control.Event{Thread: id, Kind: "machine_action", Time: time.Now(), Data: data}); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM pending_inputs WHERE id=? AND session_id=?`, pending, sid)
	return err
}

func (r *Runner) resolveActionInputs(lv *live, raw, state string) error {
	return r.recordActionResolution(lv, 0, raw, state)
}

// The correlated receipt and all local delivery markers form one commit.
// Missing, pending and unknown receipts never reach this path.
func (r *Runner) recordActionResolution(lv *live, action int64, raw, state string, retryUnavailable ...bool) error {
	if state != "complete" && state != "refused" {
		return machineio.ErrUnknown
	}
	var inputs []string
	if err := json.Unmarshal([]byte(raw), &inputs); err != nil {
		return err
	}
	var retryAnswer int64
	var unsent []string
	ctx := context.Background()
	err := r.Store.Update(ctx, func(tx *store.Tx) error {
		if action != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE session_machine_actions SET state=? WHERE id=?`, state, action); err != nil {
				return err
			}
		}
		for _, in := range inputs {
			if value, ok := strings.CutPrefix(in, "answer:"); ok {
				value, key, valid := strings.Cut(value, ":")
				if !valid || key == "" {
					return errors.New("runner: recorded answer has no receipt identity")
				}
				id, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return err
				}
				var matches int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_inputs WHERE id=? AND session_id=? AND receipt_key=?`, id, lv.id, key).Scan(&matches); err != nil {
					return err
				}
				if matches == 0 {
					continue
				}
				if state == "complete" {
					if _, err := tx.ExecContext(ctx, `DELETE FROM pending_inputs WHERE id=? AND session_id=?`, id, lv.id); err != nil {
						return err
					}
				} else if len(retryUnavailable) > 0 && retryUnavailable[0] {
					// A correlated transient no-write proof permits another
					// attempt only after reconstruction, hook and authority checks.
					retryAnswer = max(retryAnswer, id)
				} else if err := r.refusePendingAnswerTx(ctx, tx, lv.id, id); err != nil {
					return err
				}
			} else if state == "refused" {
				unsent = append(unsent, in)
			}
		}
		return r.setWrittenTx(ctx, tx, lv, unsent, false, nil)
	})
	if err != nil {
		return err
	}
	if err := r.Store.DB.QueryRow(`SELECT last_seq FROM sessions WHERE id=?`, lv.id).Scan(&lv.seq); err != nil {
		return err
	}
	if retryAnswer != 0 {
		lv.pending = true
		lv.upTo = max(lv.upTo, retryAnswer)
	}
	if len(unsent) > 0 || retryAnswer != 0 {
		lv.warmPending = true
		return core.ErrMachineUnchecked
	}
	return nil
}
