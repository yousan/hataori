package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// WorkerLogin identifies the original outage, never a main-local login.
type WorkerLogin struct {
	Machine string
	Agent   string
	Dir     string
	Open    bool
}

func WorkerOutage(ctx context.Context, q row, id int64) (WorkerLogin, bool, error) {
	var l WorkerLogin
	err := q.QueryRowContext(ctx, `SELECT machine,agent,config_dir,closed_at IS NULL FROM login_outages WHERE id=? AND machine!='local'`, id).
		Scan(&l.Machine, &l.Agent, &l.Dir, &l.Open)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerLogin{}, false, nil
	}
	return l, err == nil, err
}

// CloseWorkerLoginOutage closes only this original outage while its identity
// still matches. A later outage of the same login is deliberately untouched.
func CloseWorkerLoginOutage(ctx context.Context, tx *store.Tx, id int64, expected WorkerLogin) (bool, error) {
	l, ok, err := WorkerOutage(ctx, tx, id)
	if err != nil || !ok || !l.Open || l.Machine != expected.Machine || l.Agent != expected.Agent || l.Dir != expected.Dir {
		return false, err
	}
	var thread control.ThreadID
	err = tx.QueryRowContext(ctx, `SELECT thread_id FROM login_outage_threads WHERE outage_id=? ORDER BY thread_id LIMIT 1`, id).Scan(&thread)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return true, closeOutage(ctx, tx, l.Agent, id, thread, time.Now().UnixMilli())
}
