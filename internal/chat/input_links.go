package chat

import (
	"context"
	"fmt"

	"github.com/yousan/hataori/internal/store"
)

// RevokeUnservedInputs records which stored links the loaded configuration
// no longer serves. It runs before any driver or identity check, including
// for clients with no configured driver. It only closes input boundaries:
// restoring them and changing the outbox still require a verified front end.
func RevokeUnservedInputs(ctx context.Context, st *store.Store, serves func(client, channel, scope string) bool) error {
	err := st.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT l.id, l.client, l.channel, t.scope FROM links l
			JOIN threads t ON t.id = l.thread_id WHERE l.since <> ?`, neverSince)
		if err != nil {
			return err
		}
		defer rows.Close()
		var revoked []int64
		for rows.Next() {
			var id int64
			var client, channel, scope string
			if err := rows.Scan(&id, &client, &channel, &scope); err != nil {
				return err
			}
			if !serves(client, channel, scope) {
				revoked = append(revoked, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range revoked {
			if _, err := tx.ExecContext(ctx, `UPDATE links SET since = ? WHERE id = ?`, neverSince, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("chat: revoking unserved link inputs: %w", err)
	}
	return nil
}
