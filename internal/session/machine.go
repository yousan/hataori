package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// Machine applies Step to sessions in a store.
type Machine struct {
	Store *store.Store

	// beforeCommit, when set, runs inside the transaction after everything
	// is written; an error rolls it all back. For tests.
	beforeCommit func() error
}

// Apply feeds one input to a session and commits the new state, the turn
// records and the events together. An agent event whose sequence number
// the session has already seen is ignored and writes nothing. A refused
// input returns an error wrapping ErrRefused and writes nothing.
func (m *Machine) Apply(ctx context.Context, sessionID int64, in Input) error {
	return m.Store.Update(ctx, func(tx *store.Tx) error {
		if err := m.ApplyTx(ctx, tx, sessionID, in); err != nil {
			return err
		}
		if m.beforeCommit != nil {
			return m.beforeCommit()
		}
		return nil
	})
}

// ApplyTx is Apply inside a transaction the caller owns, so that a caller
// can commit other rows, such as the inbox entry of the call, with it.
func (m *Machine) ApplyTx(ctx context.Context, tx *store.Tx, sessionID int64, in Input) error {
	var (
		state   string
		lastSeq uint64
		thread  int64
		id      control.ThreadID
	)
	err := tx.QueryRowContext(ctx, `
		SELECT s.state, s.last_seq, t.id, t.scope, t.key
		FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE s.id = ?`, sessionID).Scan(&state, &lastSeq, &thread, &id.Scope, &id.Key)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session: no session %d", sessionID)
	}
	if err != nil {
		return fmt.Errorf("session: load %d: %w", sessionID, err)
	}
	if in.Kind == AgentEvent && in.Agent.Seq <= lastSeq {
		return nil // seen before
	}

	snap := Snapshot{State: control.State(state)}
	var turnID int64
	var t OpenTurn
	err = tx.QueryRowContext(ctx, `SELECT id, started_by, principal FROM turns WHERE session_id = ? AND ended_at IS NULL`,
		sessionID).Scan(&turnID, &t.By, &t.Principal)
	switch {
	case err == nil:
		snap.Turn = &t
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("session: load turn: %w", err)
	}

	r, err := Step(snap, in)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()

	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, updated_at = ? WHERE id = ?`,
		string(r.State), now, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ?`,
		string(r.State), now, thread); err != nil {
		return err
	}
	if in.Kind == AgentEvent {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seq = ? WHERE id = ?`, in.Agent.Seq, sessionID); err != nil {
			return err
		}
		if e := in.Agent; e.Kind == agent.SessionReady {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id = ?, model = ?, effort = ? WHERE id = ?`,
				e.SessionID, e.Model, e.Effort, sessionID); err != nil {
				return err
			}
		}
	}
	if r.End != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET ended_at = ?, status = ?, reason = ?, background = ? WHERE id = ?`,
			now, r.End.Outcome, r.End.Reason, r.End.Background, turnID); err != nil {
			return err
		}
	}
	endedTurn := turnID
	if r.Start != nil {
		res, err := tx.ExecContext(ctx, `INSERT INTO turns (session_id, started_by, principal, trigger, started_at) VALUES (?, ?, ?, ?, ?)`,
			sessionID, r.Start.By, string(r.Start.Principal), r.Key, now)
		if err != nil {
			return err
		}
		if turnID, err = res.LastInsertId(); err != nil {
			return err
		}
	}

	for _, o := range r.Out {
		data := map[string]any{"session": sessionID}
		for k, v := range o.Data {
			data[k] = v
		}
		switch o.Kind {
		case KindTurnEnded:
			data["turn"] = endedTurn
		case KindTurnStarted, KindInput, KindAnswer:
			data["turn"] = turnID
		}
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := tx.Append(ctx, control.Event{Thread: id, Kind: o.Kind, Data: b}); err != nil {
			return err
		}
	}
	return nil
}
