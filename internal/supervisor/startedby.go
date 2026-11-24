package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yousan/hataori/internal/control"
)

// StartedBy returns the supervisor thread of scope that started a thread
// whose thread_created event, at position pos, names from: the scope's
// confined thread that was open then, else its newest one before it; 0
// when from is not the scope's supervisor (ID).
func StartedBy(ctx context.Context, db *sql.DB, scope, from string, pos uint64) (control.ThreadID, error) {
	if from != ID(scope) {
		return 0, nil
	}
	// A scope has one open at a time, so the one that was not closed by
	// then is the one that called; a reopened one is open again.
	var id int64
	err := db.QueryRowContext(ctx, `SELECT t.id FROM threads t
		JOIN events c ON c.thread_id = t.id AND c.kind = 'thread_created' AND c.pos < ?
		WHERE t.scope = ? AND t.confined = 1
		ORDER BY coalesce((SELECT json_extract(CAST(s.data AS TEXT), '$.to') FROM events s
			WHERE s.thread_id = t.id AND s.kind = 'state' AND s.pos < ? ORDER BY s.pos DESC LIMIT 1), '') IN (?, ?), t.id DESC LIMIT 1`,
		pos, scope, pos, string(control.Closed), string(control.Removed)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("supervisor: who started the thread: %w", err)
	}
	return control.ThreadID(id), nil
}
