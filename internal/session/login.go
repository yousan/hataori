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

// Login is one login of an agent: the one in a configuration directory, ""
// for the one of a session with no directory of its own (agent.LoginDir).
// Sessions of one login are logged out, and back, together.
type Login struct {
	Agent string
	Dir   string
}

// row is the part of a database or a transaction that reads one row.
type row interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// OutageLogin returns the login whose outage is id; ok is false when there
// is no such outage.
func OutageLogin(ctx context.Context, q row, id int64) (l Login, ok bool, err error) {
	// Remote login relay is not implemented yet. A remote outage must never
	// cause the current relay to run a login command on the main machine.
	err = q.QueryRowContext(ctx, `SELECT agent, config_dir FROM login_outages WHERE id = ? AND machine = 'local'`, id).Scan(&l.Agent, &l.Dir)
	if errors.Is(err, sql.ErrNoRows) {
		return Login{}, false, nil
	}
	if err != nil {
		return Login{}, false, fmt.Errorf("session: login outage: %w", err)
	}
	return l, true, nil
}

// OpenOutage returns the open outage of the login; 0 when there is none.
func OpenOutage(ctx context.Context, q row, l Login) (int64, error) {
	return openMachineOutage(ctx, q, l, store.LocalMachine)
}

func openMachineOutage(ctx context.Context, q row, l Login, machine string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM login_outages WHERE machine = ? AND agent = ? AND config_dir = ? AND closed_at IS NULL`,
		machine, l.Agent, l.Dir).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("session: login outage: %w", err)
	}
	return id, nil
}

// login is the login of a session of the agent.
func (m *Machine) login(ctx context.Context, tx *store.Tx, agentName string, sessionID int64) (Login, error) {
	var stored string
	var machine string
	if err := tx.QueryRowContext(ctx, `SELECT s.spec, t.machine FROM sessions s JOIN threads t ON t.id = s.thread_id WHERE s.id = ?`, sessionID).Scan(&stored, &machine); err != nil {
		return Login{}, fmt.Errorf("session: login: %w", err)
	}
	// '' before the session's first launch. A spec that cannot be read is
	// one without a directory: the turn's end is kept whatever it is.
	var s struct{ Spec struct{ ConfigDir string } }
	json.Unmarshal([]byte(stored), &s)
	dir := s.Spec.ConfigDir
	if machine == store.LocalMachine && m.LoginDir != nil {
		dir = m.LoginDir(agentName, agent.Spec{ConfigDir: dir})
	}
	return Login{Agent: agentName, Dir: dir}, nil
}

// recordLogin keeps the login outage of a session's login: a turn that fails
// with reason login opens it (or joins the open one), and a turn of the same
// login that ends without a failure closes it. It runs in the transaction
// of the turn's end.
func (m *Machine) recordLogin(ctx context.Context, tx *store.Tx, agentName string, sessionID, thread int64, id control.ThreadID, e Ended) error {
	l, err := m.login(ctx, tx, agentName, sessionID)
	if err != nil {
		return err
	}
	var machine string
	if err := tx.QueryRowContext(ctx, `SELECT t.machine FROM sessions s JOIN threads t ON t.id = s.thread_id WHERE s.id = ?`, sessionID).Scan(&machine); err != nil {
		return err
	}
	outage, err := openMachineOutage(ctx, tx, l, machine)
	if err != nil {
		return err
	}
	open := outage != 0
	now := time.Now().UnixMilli()

	switch {
	case e.Outcome == Failure && e.Reason == string(agent.Login):
		if !open {
			res, err := tx.ExecContext(ctx, `INSERT INTO login_outages (machine, agent, config_dir, opened_at) VALUES (?, ?, ?, ?)`, machine, l.Agent, l.Dir, now)
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

// CloseLoginOutage closes the login's open outage, if there is one, and
// writes login_restored on the first thread it stopped. It reports whether an
// outage was open. The login relay calls it once a login of its own has
// succeeded.
func CloseLoginOutage(ctx context.Context, tx *store.Tx, l Login) (bool, error) {
	outage, err := OpenOutage(ctx, tx, l)
	if err != nil || outage == 0 {
		return false, err
	}
	var id control.ThreadID
	err = tx.QueryRowContext(ctx, `SELECT thread_id FROM login_outage_threads
		WHERE outage_id = ? ORDER BY thread_id LIMIT 1`, outage).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("session: login outage: %w", err)
	}
	return true, closeOutage(ctx, tx, l.Agent, outage, id, time.Now().UnixMilli())
}

// closeOutage closes an open outage and writes login_restored on id.
func closeOutage(ctx context.Context, tx *store.Tx, agentName string, outage int64, id control.ThreadID, now int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.scope FROM login_outage_threads o JOIN threads t ON t.id = o.thread_id
		WHERE o.outage_id = ? ORDER BY t.id`, outage)
	if err != nil {
		return fmt.Errorf("session: login outage: %w", err)
	}
	var (
		stopped []control.ThreadID
		scopes  []string // "scopes" lets a reader withhold the event from a caller that may not see every thread it names
	)
	for rows.Next() {
		var (
			t     control.ThreadID
			scope string
		)
		if err := rows.Scan(&t, &scope); err != nil {
			rows.Close()
			return err
		}
		stopped = append(stopped, t)
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `UPDATE login_outages SET closed_at = ? WHERE id = ?`, now, outage); err != nil {
		return fmt.Errorf("session: close login outage: %w", err)
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
