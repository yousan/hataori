package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Tx is a write transaction. Inbox rows and events written through it are
// committed together with everything else in it, or not at all.
type Tx struct {
	*sql.Tx
	appended bool
}

// Update runs fn in one write transaction and commits it when fn returns
// nil. Readers following the event log are woken after the commit.
func (s *Store) Update(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer sqlTx.Rollback() // no-op after Commit; frees the connection on a panic
	tx := &Tx{Tx: sqlTx}
	if err := fn(tx); err != nil {
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	if tx.appended {
		s.notify()
	}
	return nil
}

func (s *Store) notify() {
	s.mu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

func (s *Store) waiter() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

// Accept stores a call under its key. It reports false, and stores
// nothing, when the key was accepted before.
func (tx *Tx) Accept(ctx context.Context, key string, data []byte) (bool, error) {
	if data == nil {
		data = []byte{}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO inbox (key, data, accepted_at) VALUES (?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		key, data, time.Now().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("store: accept: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: accept: %w", err)
	}
	return n == 1, nil
}

// Append adds e to the event log and returns its position. e.Pos is
// ignored; e.Time is set to now when it is zero.
func (tx *Tx) Append(ctx context.Context, e control.Event) (uint64, error) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	data := []byte(e.Data)
	if data == nil {
		data = []byte{}
	}
	var pos uint64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO events (scope, thread, time, kind, data) VALUES (?, ?, ?, ?, ?) RETURNING pos`,
		e.Thread.Scope, e.Thread.Key, e.Time.UnixMilli(), string(e.Kind), data).Scan(&pos)
	if err != nil {
		return 0, fmt.Errorf("store: append event: %w", err)
	}
	tx.appended = true
	return pos, nil
}

// Events returns up to limit committed events after position after that
// match f, in order.
func (s *Store) Events(ctx context.Context, f control.Filter, after uint64, limit int) ([]control.Event, error) {
	q := `SELECT pos, scope, thread, time, kind, data FROM events WHERE pos > ?`
	args := []any{after}
	if f.Scope != "" {
		q += ` AND scope = ?`
		args = append(args, f.Scope)
	}
	if f.Thread != (control.ThreadID{}) {
		q += ` AND scope = ? AND thread = ?`
		args = append(args, f.Thread.Scope, f.Thread.Key)
	}
	q += ` ORDER BY pos LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	var out []control.Event
	for rows.Next() {
		var (
			e    control.Event
			ms   int64
			kind string
			data []byte
		)
		if err := rows.Scan(&e.Pos, &e.Thread.Scope, &e.Thread.Key, &ms, &kind, &data); err != nil {
			return nil, fmt.Errorf("store: read events: %w", err)
		}
		e.Time = time.UnixMilli(ms)
		e.Kind = control.EventKind(kind)
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	return out, nil
}

// Follow sends every committed event after position after that matches f
// to out, in order and once each, then live ones, until ctx ends. It
// returns ctx's error, or the error that stopped it.
func (s *Store) Follow(ctx context.Context, f control.Filter, after uint64, out chan<- control.Event) error {
	const batch = 500
	for {
		// Take the waiter before reading, so a commit between the read
		// and the wait is not missed.
		wake := s.waiter()
		evs, err := s.Events(ctx, f, after, batch)
		if err != nil {
			return err
		}
		for _, e := range evs {
			select {
			case out <- e:
				after = e.Pos
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(evs) == batch {
			continue
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
