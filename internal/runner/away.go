package runner

import (
	"context"
	"errors"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
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
