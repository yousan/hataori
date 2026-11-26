package core

import (
	"context"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// home records the confined connection that actually gave a worker input.
// A person's input and a repeated or refused call leave its home alone.
// The caller supplies no home over the socket: AsksIn is daemon-bound.
func (a *api) home(ctx context.Context, tx *store.Tx, id control.ThreadID, scope string) error {
	if a.caller.AsksIn == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO thread_homes (thread, after, home)
		SELECT ?, coalesce((SELECT max(pos) FROM events), 0), origin.id
		FROM threads origin JOIN threads worker ON worker.id = ?
		WHERE origin.id = ? AND origin.scope = ? AND worker.scope = origin.scope
		AND origin.kind IN ('supervisor', 'conversation') AND worker.kind = 'ordinary'
		ON CONFLICT (thread, after) DO UPDATE SET home = excluded.home`,
		int64(id), int64(id), int64(a.caller.AsksIn), scope)
	return err
}
