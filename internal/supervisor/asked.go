package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
)

// A thread the supervisor starts during a turn that a person's message
// started is asked for by that person, who waits for its answer (design,
// section 7.1, the budget; #677): its first wake is not held by the hourly
// limits. The mark is kept in supervisor_asked under the key of the start,
// written before the start is made, so that a daemon that dies in between
// does not lose it, and so that it holds for a start that is a proposal,
// which a person approves later. It is cleared by the thread's first wake:
// woken is then the position that wake covered.

// asking is api, the API of scope's supervisor in thread, with each start
// made during a person's turn marked.
type asking struct {
	control.API
	core   *core.Core
	scope  string
	thread control.ThreadID // the supervisor's
	keys   *keyed           // one start of a key at a time, so that a mark is one call's
	log    *slog.Logger
}

func (a *asking) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	if r.Scope != a.scope || r.Key == "" {
		return a.API.Start(ctx, r)
	}
	defer a.keys.lock(a.scope + "\x00" + r.Key)()
	marked, err := a.mark(ctx, r.Key)
	if err != nil {
		// The start goes on: without its mark, the thread wakes as any.
		a.log.Error("supervisor: marking a start asked for by a person", "scope", a.scope, "err", err)
	}
	id, startErr := a.API.Start(ctx, r)
	if marked && startErr != nil && !errors.Is(startErr, control.ErrProposed) {
		// Refused or failed: unless it made a thread all the same, as one
		// whose launch failed, there is nothing to mark.
		made, proposed, err := a.core.StartMade(ctx, a.scope, r.Key, a.thread)
		if err == nil && made == 0 && proposed == "" {
			err = unmark(ctx, a.core, a.scope, r.Key)
		}
		if err != nil {
			a.log.Error("supervisor: the mark of a start that failed", "scope", a.scope, "err", err)
		}
	}
	return id, startErr
}

// mark records the start under key as asked for, when a person's message
// started the turn the supervisor runs. A key that started a thread, or
// made a proposal, before is left as it was. Outside a person's turn, a
// mark left by a call that a daemon died in before it made anything goes.
func (a *asking) mark(ctx context.Context, key string) (bool, error) {
	made, proposed, err := a.core.StartMade(ctx, a.scope, key, a.thread)
	if err != nil || made != 0 || proposed != "" {
		return false, err
	}
	person, ok, err := personsTurn(ctx, a.core, a.scope, a.thread)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, unmark(ctx, a.core, a.scope, key)
	}
	err = a.core.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO supervisor_asked (scope, key, asks_in, person) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			a.scope, key, int64(a.thread), person)
		return err
	})
	return err == nil, err
}

// keyed is a lock for each key.
type keyed struct {
	mu   sync.Mutex
	held map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	n int // who holds it or waits for it
}

// lock takes the lock of key, and returns what gives it back.
func (k *keyed) lock(key string) func() {
	k.mu.Lock()
	if k.held == nil {
		k.held = map[string]*keyLock{}
	}
	l := k.held[key]
	if l == nil {
		l = &keyLock{}
		k.held[key] = l
	}
	l.n++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.n--; l.n == 0 {
			delete(k.held, key)
		}
		k.mu.Unlock()
	}
}

// unmark removes the mark of the start under key.
func unmark(ctx context.Context, c *core.Core, scope, key string) error {
	return c.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM supervisor_asked WHERE scope = ? AND key = ?`, scope, key)
		return err
	})
}

// personsTurn reports whether thread, a supervisor's of scope, runs a turn
// that a person started, and who: by a message or an answer, any input but
// those hataori gives there (startedKeyPrefix) and the supervisor's own,
// or at its terminal.
func personsTurn(ctx context.Context, c *core.Core, scope string, thread control.ThreadID) (string, bool, error) {
	var kind string
	var data []byte
	err := c.Store.DB.QueryRowContext(ctx, `SELECT kind, data FROM events WHERE thread_id = ? AND kind IN ('turn_started', 'turn_ended')
		ORDER BY pos DESC LIMIT 1`, int64(thread)).Scan(&kind, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("supervisor: the running turn: %w", err)
	}
	if kind != "turn_started" {
		return "", false, nil
	}
	var d struct {
		By        string `json:"by"`
		Key       string `json:"key"`
		From      string `json:"from"`
		Principal string `json:"principal"`
	}
	json.Unmarshal(data, &d) // the store checked the event against its schema
	switch d.By {
	case "terminal":
		return d.Principal, true, nil
	case "caller":
		return d.Principal, !strings.HasPrefix(d.Key, startedKeyPrefix) && d.From != ID(scope), nil
	}
	return "", false, nil
}

// askedRow is a mark not cleared yet, and the thread its start made.
type askedRow struct {
	key    string
	thread control.ThreadID // 0 for none yet
}

// askedRows returns the marks of scope not cleared yet. A mark whose
// proposal was decided and made no thread is removed.
func askedRows(ctx context.Context, c *core.Core, scope string) ([]askedRow, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT key, asks_in FROM supervisor_asked WHERE scope = ? AND woken IS NULL`, scope)
	if err != nil {
		return nil, fmt.Errorf("supervisor: threads asked for by a person: %w", err)
	}
	type row struct {
		key string
		in  int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.in); err != nil {
			rows.Close()
			return nil, fmt.Errorf("supervisor: threads asked for by a person: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("supervisor: threads asked for by a person: %w", err)
	}
	var out []askedRow
	for _, r := range all {
		id, proposed, err := c.StartMade(ctx, scope, r.key, control.ThreadID(r.in))
		if err != nil {
			return nil, err
		}
		switch {
		case id != 0 || proposed == "" || proposed == "open" || proposed == "approved":
			out = append(out, askedRow{r.key, id})
		default:
			// Decided, and it made no thread: it never will.
			if err := unmark(ctx, c, scope, r.key); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// asked returns the threads of the scope that are marked.
func (r *waking) asked(ctx context.Context) (map[control.ThreadID]bool, error) {
	rows, err := askedRows(ctx, r.w.Core, r.w.Scope)
	if err != nil {
		return nil, err
	}
	out := map[control.ThreadID]bool{}
	for _, a := range rows {
		if a.thread != 0 {
			out[a.thread] = true
		}
	}
	return out, nil
}

// wokenTo returns the position that the first wake of thread id, marked
// when it was made, covered; 0 when none did. The waker's position may be
// before it: that wake left the turn ends a gate held.
func (r *waking) wokenTo(ctx context.Context, id control.ThreadID) (uint64, error) {
	var to sql.NullInt64
	if err := r.w.Core.Store.DB.QueryRowContext(ctx, `SELECT max(woken) FROM supervisor_asked WHERE scope = ? AND thread = ?`,
		r.w.Scope, int64(id)).Scan(&to); err != nil {
		return 0, fmt.Errorf("supervisor: threads asked for by a person: %w", err)
	}
	return uint64(to.Int64), nil
}

// carries returns what records, in the transaction that stores a wake to
// be sent, that it carries the marks of threads: delivered clears those,
// shown in the digest or only counted there, also after a restart.
func (r *waking) carries(ctx context.Context, key string, threads []Ended) (func(*store.Tx) error, error) {
	rows, err := askedRows(ctx, r.w.Core, r.w.Scope)
	if err != nil {
		return nil, err
	}
	in := map[control.ThreadID]bool{}
	for _, t := range threads {
		in[t.Thread] = true
	}
	return func(tx *store.Tx) error {
		if err := r.drops(ctx)(tx); err != nil {
			return err
		}
		for _, a := range rows {
			if a.thread == 0 || !in[a.thread] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE supervisor_asked SET thread = ?, wake = ? WHERE scope = ? AND key = ?`,
				int64(a.thread), key, r.w.Scope, a.key); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// drops returns what forgets the marks a stored wake carries, as it is
// made again.
func (r *waking) drops(ctx context.Context) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_asked SET wake = NULL WHERE scope = ? AND woken IS NULL`, r.w.Scope)
		return err
	}
}

// renames returns what gives the stored wake's marks its new key.
func (r *waking) renames(ctx context.Context, from, to string) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_asked SET wake = ? WHERE scope = ? AND wake = ?`, to, r.w.Scope, from)
		return err
	}
}

// clears returns what clears the marks that the wake under key carries,
// in the transaction that stores it as given, up to to.
func (r *waking) clears(ctx context.Context, key string, to uint64) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_asked SET woken = ? WHERE scope = ? AND wake = ? AND woken IS NULL`,
			int64(to), r.w.Scope, key)
		return err
	}
}
