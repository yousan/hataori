package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Tx is a write transaction. Inbox rows and events written through it are
// committed together with everything else in it, or not at all.
type Tx struct {
	*sql.Tx
	appended bool
	check    func(control.Event) error
}

// Update runs fn in one write transaction and commits it when fn returns
// nil. Readers following the event log are woken after the commit.
func (s *Store) Update(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer sqlTx.Rollback() // no-op after Commit; frees the connection on a panic
	tx := &Tx{Tx: sqlTx, check: s.CheckEvent}
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
// ignored; e.Time is set to now when it is zero. An event of a thread is
// stored with the thread's scope; e.Scope, when set, must be that scope.
func (tx *Tx) Append(ctx context.Context, e control.Event) (uint64, error) {
	var thread any // NULL for an event of no thread
	if e.Thread != 0 {
		var scope string
		if err := tx.QueryRowContext(ctx, `SELECT scope FROM threads WHERE id = ?`, int64(e.Thread)).Scan(&scope); err != nil {
			return 0, fmt.Errorf("store: append event: thread %d: %w", e.Thread, err)
		}
		if e.Scope != "" && e.Scope != scope {
			return 0, fmt.Errorf("store: append event: thread %d is in scope %q, not %q", e.Thread, scope, e.Scope)
		}
		e.Scope, thread = scope, int64(e.Thread)
	}
	if tx.check != nil {
		if err := tx.check(e); err != nil {
			return 0, fmt.Errorf("store: append event: %w", err)
		}
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	data := []byte(e.Data)
	if data == nil {
		data = []byte{}
	}
	var pos uint64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO events (scope, thread, thread_id, time, kind, data) VALUES (?, '', ?, ?, ?, ?) RETURNING pos`,
		e.Scope, thread, e.Time.UnixMilli(), string(e.Kind), data).Scan(&pos)
	if err != nil {
		return 0, fmt.Errorf("store: append event: %w", err)
	}
	tx.appended = true
	return pos, nil
}

// Events returns up to limit committed events after position after that
// match f, in order.
func (s *Store) Events(ctx context.Context, f control.Filter, after uint64, limit int) ([]control.Event, error) {
	q := `SELECT pos, scope, coalesce(thread_id, 0), time, kind, data FROM events WHERE pos > ?`
	args := []any{after}
	if f.Scope != "" {
		q += ` AND scope = ?`
		args = append(args, f.Scope)
	}
	if f.Thread != 0 {
		q += ` AND thread_id = ?`
		args = append(args, int64(f.Thread))
	}
	if l := f.Link; l != nil {
		q += ` AND thread_id = (SELECT thread_id FROM links WHERE client = ? AND channel = ? AND thread = ?)`
		args = append(args, l.Client, l.Channel, l.Thread)
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
		if err := rows.Scan(&e.Pos, &e.Scope, &e.Thread, &ms, &kind, &data); err != nil {
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

// Last returns the last n committed events of a thread that keep accepts,
// newest last.
func (s *Store) Last(ctx context.Context, id control.ThreadID, n int, keep func(control.Event) bool) ([]control.Event, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT pos, scope, time, kind, data FROM events
		WHERE thread_id = ? ORDER BY pos DESC`, int64(id))
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	var out []control.Event
	for len(out) < n && rows.Next() {
		var (
			e    = control.Event{Thread: id}
			ms   int64
			kind string
			data []byte
		)
		if err := rows.Scan(&e.Pos, &e.Scope, &ms, &kind, &data); err != nil {
			return nil, fmt.Errorf("store: read events: %w", err)
		}
		e.Time = time.UnixMilli(ms)
		e.Kind = control.EventKind(kind)
		e.Data = json.RawMessage(data)
		if keep(e) {
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}

// Follow sends every committed event after position after that matches f
// to out, in order and once each, then live ones, until ctx ends. It
// returns ctx's error, or the error that stopped it.
func (s *Store) Follow(ctx context.Context, f control.Filter, after uint64, out chan<- control.Event) error {
	return s.FollowMark(ctx, f, after, out, nil)
}

// FollowMark is Follow, and calls caughtUp once, with the position of the
// last event sent, when every event stored before the call has been sent.
func (s *Store) FollowMark(ctx context.Context, f control.Filter, after uint64, out chan<- control.Event, caughtUp func(pos uint64)) error {
	const batch = 500
	var head uint64 // the last position stored when the call was made
	if caughtUp != nil {
		if err := s.DB.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM events`).Scan(&head); err != nil {
			return err
		}
	}
	mark := func() {
		if caughtUp != nil {
			caughtUp(after)
			caughtUp = nil
		}
	}
	for {
		// Take the waiter before reading, so a commit between the read
		// and the wait is not missed.
		wake := s.waiter()
		evs, err := s.Events(ctx, f, after, batch)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.Pos > head {
				mark() // the first event stored after the call
			}
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
		mark()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
