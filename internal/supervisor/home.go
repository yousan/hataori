package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/yousan/hataori/internal/control"
)

// homes starts an independent reader for each conversation. A reader keeps
// owning its pending events when its conversation closes, and delivers those
// to the own thread. This avoids a race between a shared cursor and closure.
func (w *Waker) homes(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	if err := Begin(ctx, w.Core, w.Scope); err != nil {
		return err
	}
	seen := map[control.ThreadID]bool{}
	start := func(id control.ThreadID) {
		if seen[id] {
			return
		}
		seen[id] = true
		child := &Waker{Core: w.Core, Scope: w.Scope, Owner: w.Owner, Quiet: w.Quiet,
			OutputMax: w.OutputMax, ThreadsMax: w.ThreadsMax, Gates: w.Gates, Clock: w.Clock,
			Log: w.Log, Budget: w.Budget, Tell: w.Tell, Before: w.Before,
			Hook: w.Hook, Sleep: w.Sleep, target: id}
		wg.Add(1)
		go func() { defer wg.Done(); child.Run(ctx) }()
	}
	var after uint64
	if err := w.Core.Store.DB.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM events`).Scan(&after); err != nil {
		return err
	}
	rows, err := w.Core.Store.DB.QueryContext(ctx, `SELECT id FROM threads WHERE scope = ? AND kind = ?`, w.Scope, string(control.KindConversation))
	if err != nil {
		return err
	}
	var ids []control.ThreadID
	for rows.Next() {
		var id control.ThreadID
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
	for _, id := range ids {
		start(id)
	}
	api := w.Core.As(control.Caller{Principal: w.Owner, Scopes: []string{w.Scope}, May: control.OpRead})
	events, err := api.Events(ctx, control.Filter{Scope: w.Scope}, after)
	if err != nil {
		return err
	}
	for e := range events {
		if e.Kind != "thread_created" {
			continue
		}
		kind, err := w.Core.Kind(ctx, e.Thread)
		if err != nil {
			return err
		}
		if kind == control.KindConversation {
			start(e.Thread)
		}
	}
	return ctx.Err()
}

// homeOf is the immutable reader that owns a worker event. Its delivery
// target may fall back later, without handing the event to another reader.
func (w *Waker) homeOf(ctx context.Context, e control.Event) (control.ThreadID, error) {
	var home control.ThreadID
	err := w.Core.Store.DB.QueryRowContext(ctx, `SELECT CASE WHEN t.kind = ? THEN h.home ELSE 0 END
		FROM thread_homes h JOIN threads t ON t.id = h.home
		WHERE h.thread = ? AND h.after < ?
		ORDER BY h.after DESC LIMIT 1`, string(control.KindConversation), int64(e.Thread), int64(e.Pos)).Scan(&home)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return home, err
}

func (r *waking) destination(ctx context.Context) (control.ThreadID, control.State, error) {
	if r.w.target != 0 && (r.sending == nil || !r.sending.fallback) {
		var state control.State
		err := r.w.Core.Store.DB.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ? AND scope = ? AND kind = ?`,
			int64(r.w.target), r.w.Scope, string(control.KindConversation)).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, "", fmt.Errorf("supervisor: wake home: %w", err)
		}
		if err == nil && state != control.Closed && state != control.Removed && state != control.Failed {
			return r.w.target, state, nil
		}
	}
	return r.w.Core.SupervisorThread(ctx, r.w.Scope)
}

// readerKey preserves an immutable reader identity when a wake is retargeted.
func readerKey(key, previous string) string {
	if _, origin, ok := strings.Cut(previous, "~"); ok {
		return key + "~" + origin
	}
	return key
}

func sameReader(key string, target control.ThreadID) bool {
	_, origin, _ := strings.Cut(key, "~")
	if target == 0 {
		return origin == ""
	}
	return origin == strconv.FormatInt(int64(target), 10)
}

// accepted distinguishes input kept for a conversation place or an agent
// launch from a wake that actually started a turn. A closed home drops its
// queued input; its reader must still deliver that work to the own thread.
func (r *waking) accepted(ctx context.Context) (delivered, waiting bool, err error) {
	id, took, err := r.w.Core.SentTo(ctx, r.w.Scope, r.sending.key)
	if err != nil || !took {
		return false, false, err
	}
	if id != r.w.target {
		// Fallback retains the own waker's acceptance rule. A failed fresh
		// launch restores its asleep conversation and reports an unread wake;
		// it must not leave this reader waiting for a turn that cannot start.
		return true, false, nil
	}
	var state control.State
	// Read delivery and queued/launch ownership in one snapshot: an agent
	// becoming ready between separate reads must not look like dropped input.
	err = r.w.Core.Store.DB.QueryRowContext(ctx, `SELECT state,
		EXISTS (SELECT 1 FROM events WHERE thread_id = t.id AND kind = 'turn_started' AND json_extract(data, '$.key') = ?),
		EXISTS (SELECT 1 FROM waiting_inputs WHERE thread_id = t.id AND json_extract(input, '$.key') = ?)
		OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = (SELECT max(id) FROM sessions WHERE thread_id = t.id)
			AND s.state = 'creating' AND CASE WHEN json_valid(s.first_input) THEN json_extract(s.first_input, '$.key') END = ?)
		FROM threads t WHERE id = ?`, r.sending.key, r.sending.key, r.sending.key, int64(id)).Scan(&state, &delivered, &waiting)
	if errors.Is(err, sql.ErrNoRows) {
		r.sending.fallback = true
		return false, false, nil
	}
	if err != nil || delivered {
		return delivered, false, err
	}
	waiting = waiting && state != control.Closed && state != control.Failed && state != control.Removed
	if !waiting {
		r.sending.fallback = true
	}
	return false, waiting, nil
}
