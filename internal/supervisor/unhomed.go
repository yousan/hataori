package supervisor

import (
	"context"
	"github.com/yousan/hataori/internal/store"
)

// dropWithoutHome consumes a wake that has no destination. A stored scope
// never falls back to an own thread left by an earlier configured policy.
func (r *waking) dropWithoutHome(ctx context.Context) error {
	var changes []func(*store.Tx) error
	upto := r.read
	key := ""
	if r.sending != nil {
		upto = max(upto, r.sending.to)
		key = r.sending.key
		changes = append(changes, r.clears(ctx, r.sending.key, r.sending.to))
	}
	now := r.w.clock().Now().UnixMilli()
	changes = append(changes, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_reminders SET state=?,wake=NULL WHERE scope=? AND state=? AND (due<=? OR wake=?)`, reminderCancelled, r.w.Scope, reminderOpen, now, key)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE supervisor_wakes SET dropped=max(dropped,?) WHERE scope=?`, int64(upto), r.w.Scope)
		return err
	})
	r.sending, r.held, r.due = nil, false, false
	clear(r.stale)
	clear(r.waiting)
	r.bg = nil
	if err := r.advance(ctx, upto, changes...); err != nil {
		return err
	}
	r.w.Log.Warn("supervisor wake dropped: its home is gone", "scope", r.w.Scope)
	return r.remind(ctx)
}
