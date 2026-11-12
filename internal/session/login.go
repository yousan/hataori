package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// Event kinds of a login outage. They are written on the thread whose turn
// opened or closed the outage; their data names every thread it stopped.
const (
	KindLoginLost     control.EventKind = "login_lost"
	KindLoginRestored control.EventKind = "login_restored"
)

// recordLogin keeps the login outage of an agent: a turn that fails with
// reason login opens it (or joins the open one), and a turn of the same
// agent that ends without a failure closes it. It runs in the transaction
// of the turn's end.
func recordLogin(ctx context.Context, tx *store.Tx, agentName string, thread int64, id control.ThreadID, e Ended) error {
	var outage int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM login_outages WHERE agent = ? AND closed_at IS NULL`, agentName).Scan(&outage)
	open := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session: login outage: %w", err)
	}
	now := time.Now().UnixMilli()

	switch {
	case e.Outcome == Failure && e.Reason == string(agent.Login):
		if !open {
			res, err := tx.ExecContext(ctx, `INSERT INTO login_outages (agent, opened_at) VALUES (?, ?)`, agentName, now)
			if err != nil {
				return fmt.Errorf("session: open login outage: %w", err)
			}
			outage, _ = res.LastInsertId()
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO login_outage_threads (outage_id, thread_id) VALUES (?, ?)`,
			outage, thread); err != nil {
			return fmt.Errorf("session: login outage: %w", err)
		}
		if open {
			return nil
		}
		return appendLogin(ctx, tx, id, KindLoginLost, map[string]any{"agent": agentName, "outage": outage, "thread": thread})
	case e.Outcome != Failure && open:
		return closeOutage(ctx, tx, agentName, outage, id, now)
	}
	return nil
}

// CloseLoginOutage closes the agent's open login outage, if there is one,
// and writes login_restored on the first thread it stopped. It reports
// whether an outage was open. The login relay calls it once a login of its
// own has succeeded.
func CloseLoginOutage(ctx context.Context, tx *store.Tx, agentName string) (bool, error) {
	var outage int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM login_outages WHERE agent = ? AND closed_at IS NULL`, agentName).Scan(&outage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("session: login outage: %w", err)
	}
	var id control.ThreadID
	err = tx.QueryRowContext(ctx, `SELECT t.scope, t.key FROM login_outage_threads o JOIN threads t ON t.id = o.thread_id
		WHERE o.outage_id = ? ORDER BY t.id LIMIT 1`, outage).Scan(&id.Scope, &id.Key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("session: login outage: %w", err)
	}
	return true, closeOutage(ctx, tx, agentName, outage, id, time.Now().UnixMilli())
}

// closeOutage closes an open outage and writes login_restored on id.
func closeOutage(ctx context.Context, tx *store.Tx, agentName string, outage int64, id control.ThreadID, now int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.scope, t.key FROM login_outage_threads o JOIN threads t ON t.id = o.thread_id
		WHERE o.outage_id = ? ORDER BY t.id`, outage)
	if err != nil {
		return fmt.Errorf("session: login outage: %w", err)
	}
	var stopped []control.ThreadID
	for rows.Next() {
		var t control.ThreadID
		if err := rows.Scan(&t.Scope, &t.Key); err != nil {
			rows.Close()
			return err
		}
		stopped = append(stopped, t)
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `UPDATE login_outages SET closed_at = ? WHERE id = ?`, now, outage); err != nil {
		return fmt.Errorf("session: close login outage: %w", err)
	}
	// "scopes" lets a reader withhold the event from a caller that may
	// not see every thread it names.
	var scopes []string
	for _, t := range stopped {
		if !slices.Contains(scopes, t.Scope) {
			scopes = append(scopes, t.Scope)
		}
	}
	return appendLogin(ctx, tx, id, KindLoginRestored, map[string]any{"agent": agentName, "outage": outage, "threads": stopped, "scopes": scopes})
}

func appendLogin(ctx context.Context, tx *store.Tx, id control.ThreadID, kind control.EventKind, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, control.Event{Thread: id, Kind: kind, Data: b})
	return err
}
