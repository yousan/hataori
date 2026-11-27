package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/yousan/hataori/internal/control"
)

var awayMigration = Migration{Name: "machine absence and held actions", Up: execAll(
	`ALTER TABLE pending_inputs ADD COLUMN authority TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pending_inputs ADD COLUMN written_at INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE machine_availability(machine TEXT PRIMARY KEY REFERENCES machines(key), state TEXT NOT NULL, token TEXT NOT NULL, first_failure INTEGER NOT NULL, reason TEXT NOT NULL, epoch INTEGER NOT NULL, observed_at INTEGER NOT NULL)`,
	`CREATE TABLE machine_catchup(session_id INTEGER PRIMARY KEY REFERENCES sessions(id), token TEXT NOT NULL)`,
	`CREATE TABLE session_pauses(id INTEGER PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES sessions(id), began_at INTEGER NOT NULL, ended_at INTEGER)`,
	`CREATE UNIQUE INDEX session_pause_open ON session_pauses(session_id) WHERE ended_at IS NULL`,
	`CREATE TABLE machine_held_actions(id INTEGER PRIMARY KEY, thread_id INTEGER NOT NULL REFERENCES threads(id), session_id INTEGER NOT NULL REFERENCES sessions(id), run TEXT NOT NULL, turn INTEGER NOT NULL, request TEXT NOT NULL, kind TEXT NOT NULL, key TEXT NOT NULL, data TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, UNIQUE(thread_id,kind,key))`,
	`CREATE TABLE machine_rejected_inputs(id INTEGER PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES sessions(id), data TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL)`,
	`CREATE TABLE machine_absence_notices(machine TEXT NOT NULL REFERENCES machines(key), thread_id INTEGER NOT NULL REFERENCES threads(id), epoch INTEGER NOT NULL, PRIMARY KEY(machine,thread_id,epoch))`,
)}

type Availability struct {
	State, Token, Reason            string
	FirstFailure, Epoch, ObservedAt int64
}

func (s *Store) Availability(ctx context.Context, key string) (Availability, error) {
	var a Availability
	err := s.DB.QueryRowContext(ctx, `SELECT state,token,first_failure,reason,epoch,observed_at FROM machine_availability WHERE machine=?`, key).Scan(&a.State, &a.Token, &a.FirstFailure, &a.Reason, &a.Epoch, &a.ObservedAt)
	return a, err
}

// ObserveConnection receives only verified transport observations. A returned
// connection permits reads, never dispatch, until every owned live session has
// committed its output and reconciled its original receipts for this token.
func (s *Store) ObserveConnection(ctx context.Context, key string, connected bool, token, reason string, immediate bool, grace time.Duration, now time.Time) (Availability, error) {
	var a Availability
	err := s.Update(ctx, func(tx *Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT state,token,first_failure,reason,epoch,observed_at FROM machine_availability WHERE machine=?`, key).Scan(&a.State, &a.Token, &a.FirstFailure, &a.Reason, &a.Epoch, &a.ObservedAt)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		was := a.State
		invalidate := (!connected && a.FirstFailure == 0) || (connected && a.Token != token)
		if a.Epoch == 0 {
			a.Epoch = 1
		}
		if connected && a.State == "ready" && a.Token == token {
			return nil
		}
		if !connected {
			if a.FirstFailure == 0 {
				a.FirstFailure = now.UnixMilli()
				if was == "ready" {
					a.Epoch++
				}
			}
			a.State = "checking"
			if immediate || now.Sub(time.UnixMilli(a.FirstFailure)) >= grace {
				a.State = "away"
			}
			if a.Reason == "" || immediate {
				a.Reason = reason
			}
		} else {
			a.State = "catching_up"
			if a.Token != token && was == "ready" {
				a.Epoch++
				a.FirstFailure = now.UnixMilli()
			}
			if a.Reason == "" {
				a.Reason = "reconciling retained output and operation receipts"
			}
			a.Token = token
		}
		a.ObservedAt = now.UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO machine_availability(machine,state,token,first_failure,reason,epoch,observed_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(machine) DO UPDATE SET state=excluded.state,token=excluded.token,first_failure=excluded.first_failure,reason=excluded.reason,epoch=excluded.epoch,observed_at=excluded.observed_at`, key, a.State, a.Token, a.FirstFailure, a.Reason, a.Epoch, a.ObservedAt); err != nil {
			return err
		}
		if invalidate {
			if _, err := tx.ExecContext(ctx, `DELETE FROM machine_catchup WHERE session_id IN(SELECT s.id FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE t.machine=?)`, key); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_pauses(session_id,began_at) SELECT s.id,? FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE t.machine=? AND s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id) AND NOT EXISTS(SELECT 1 FROM session_pauses p WHERE p.session_id=s.id AND p.ended_at IS NULL)`, now.UnixMilli(), key)
		if err != nil {
			return err
		}
		if was != a.State {
			return availabilityEvents(ctx, tx, key, a, now)
		}
		return nil
	})
	return a, err
}

func availabilityEvents(ctx context.Context, tx *Tx, key string, a Availability, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM threads WHERE machine=?`, key)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"machine": key, "state": a.State, "since": a.FirstFailure, "reason": a.Reason})
	for _, id := range ids {
		if _, err := tx.Append(ctx, control.Event{Thread: control.ThreadID(id), Kind: "machine_availability", Time: now, Data: data}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AcknowledgeCatchup(ctx context.Context, sid int64, token string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO machine_catchup(session_id,token) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET token=excluded.token`, sid, token)
	return err
}

// FinishCatchup never treats an absent/unknown host as ended. Active sessions
// must each acknowledge the current verified generation; slots are untouched.
func (s *Store) FinishCatchup(ctx context.Context, key, token string, now time.Time) error {
	return s.Update(ctx, func(tx *Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM machine_availability WHERE machine=? AND token=?`, key, token).Scan(&state); err != nil {
			return err
		}
		if state != "catching_up" {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE t.machine=? AND s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id) AND s.state IN ('creating','idle','running','waiting') AND s.spec!='' AND s.launch=0 AND NOT EXISTS(SELECT 1 FROM machine_catchup c WHERE c.session_id=s.id AND c.token=?)`, key, token).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE machine_availability SET state='ready',first_failure=0,reason='',observed_at=? WHERE machine=? AND token=?`, now.UnixMilli(), key, token); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE session_pauses SET ended_at=? WHERE ended_at IS NULL AND session_id IN(SELECT s.id FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE t.machine=?)`, now.UnixMilli(), key); err != nil {
			return err
		}
		return availabilityEvents(ctx, tx, key, Availability{State: "ready"}, now)
	})
}

type RowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ActiveAge preserves real state timestamps while excluding only the overlap
// with durable pause intervals. An open interval survives main restarts.
func ActiveAge(ctx context.Context, q RowQuerier, sid int64, since, now time.Time) (time.Duration, error) {
	var paused int64
	err := q.QueryRowContext(ctx, `SELECT coalesce(sum(max(0,min(coalesce(ended_at,?),?)-max(began_at,?))),0) FROM session_pauses WHERE session_id=?`, now.UnixMilli(), now.UnixMilli(), since.UnixMilli(), sid).Scan(&paused)
	return max(time.Duration(0), now.Sub(since)-time.Duration(paused)*time.Millisecond), err
}
