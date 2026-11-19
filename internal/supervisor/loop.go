package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// LoopGuard stops a worker thread from waking its scope's supervisor
// after Cycles cycles with no person's message in between (design,
// section 7.1). A cycle on thread W is a turn of W started by the
// supervisor's input, its end, and the supervisor's wake that told it: a
// wake (a supervisor turn whose key starts with WakeKeyPrefix) that covers
// such a turn end. Several such ends in one wake are one cycle.
//
// What a person does resets W's count: in W, an input or an answer from
// anyone but the supervisor, or a turn typed at W's terminal; in a
// supervisor thread of the scope, a person's message or answer (any input
// but those hataori gives there: a wake, and the first message of the
// thread), or a turn typed at its terminal.
//
// It counts from the event log, so a restart keeps the counts. A thread
// at the count does not wake the supervisor: Admit drops its turn ends.
// Other threads keep waking it. A person is told once for each stop, in
// the supervisor's thread, naming the thread: when the wake that makes
// the last cycle was accepted (Woken), or else at the first turn end
// dropped. It is the waker's first gate, in front of the budget, so that
// what it drops spends nothing.
type LoopGuard struct {
	Core   *core.Core
	Scope  string
	Cycles int // the count; 0 turns the guard off
	// Tell is the budget's: a line in a thread, once for each key.
	Tell func(ctx context.Context, thread control.ThreadID, key, text string) error
	Log  *slog.Logger

	mu sync.Mutex
	// The supervisor threads' events read so far, for their last reset:
	// it only grows, so each read goes on from scanned.
	scanned uint64
	svReset uint64
	skip    map[turnRef]bool // turns hataori started, whose first input is not yet read
	told    map[string]bool  // notice keys told by this run
}

var (
	_ Gate  = (*LoopGuard)(nil)
	_ Woken = (*LoopGuard)(nil)
)

type turnRef struct{ session, turn int64 }

// LoopKeyPrefix starts the key of a loop guard's notice; the rest is the
// thread and the position of the last reset before it stopped.
const LoopKeyPrefix = "hataori-loop-"

// startedKeyPrefix starts the key of every input hataori itself gives a
// supervisor's thread: its first message (StartKey) and each wake
// (WakeKeyPrefix).
const startedKeyPrefix = "hataori-"

// Admit drops the threads that reached the count, and tells a person once
// for each.
func (g *LoopGuard) Admit(ctx context.Context, scope string, threads []Ended) ([]Ended, time.Time, error) {
	if g.Cycles <= 0 {
		return threads, time.Time{}, nil
	}
	svReset, err := g.supervisorReset(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	var kept []Ended
	for _, t := range threads {
		n, reset, err := g.count(ctx, t.Thread, svReset)
		if err != nil {
			return nil, time.Time{}, err
		}
		if n < g.Cycles {
			kept = append(kept, t)
			continue
		}
		g.tell(ctx, t.Thread, n, reset)
	}
	return kept, time.Time{}, nil
}

// Woken tells a person of each thread whose count the accepted wake made
// reach the limit, the wake being in the log now.
func (g *LoopGuard) Woken(ctx context.Context, scope string, threads []Ended) error {
	if g.Cycles <= 0 {
		return nil
	}
	svReset, err := g.supervisorReset(ctx)
	if err != nil {
		return err
	}
	for _, t := range threads {
		n, reset, err := g.count(ctx, t.Thread, svReset)
		if err != nil {
			return err
		}
		if n >= g.Cycles {
			g.tell(ctx, t.Thread, n, reset)
		}
	}
	return nil
}

// Count returns the cycles on thread w since the last reset, and the
// position of that reset (0 for none).
func (g *LoopGuard) Count(ctx context.Context, w control.ThreadID) (int, uint64, error) {
	svReset, err := g.supervisorReset(ctx)
	if err != nil {
		return 0, 0, err
	}
	return g.count(ctx, w, svReset)
}

// supervisorReset returns the position of the last thing a person did in
// a supervisor thread of the scope, reading on from the events read
// before: turns hataori started there have their first input left out.
func (g *LoopGuard) supervisorReset(ctx context.Context) (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.skip == nil {
		g.skip = map[turnRef]bool{}
	}
	rows, err := g.Core.Store.DB.QueryContext(ctx, `SELECT pos, kind, data FROM events
		WHERE thread_id IN (SELECT id FROM threads WHERE scope = ? AND confined = 1) AND pos > ?
		AND kind IN ('turn_started', 'input', 'answer') ORDER BY pos`, g.Scope, int64(g.scanned))
	if err != nil {
		return 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pos  int64
			kind string
			data []byte
		)
		if err := rows.Scan(&pos, &kind, &data); err != nil {
			return 0, fmt.Errorf("supervisor: loop guard: %w", err)
		}
		var d struct {
			Session int64  `json:"session"`
			Turn    int64  `json:"turn"`
			Key     string `json:"key"`
			By      string `json:"by"`
		}
		json.Unmarshal(data, &d) // the store checked it against the schema
		ref := turnRef{d.Session, d.Turn}
		switch kind {
		case "turn_started":
			if d.By == "terminal" {
				g.svReset = uint64(pos)
			} else if strings.HasPrefix(d.Key, startedKeyPrefix) {
				g.skip[ref] = true
			}
		case "input":
			if g.skip[ref] {
				delete(g.skip, ref) // hataori's own: a wake or the first message
			} else {
				g.svReset = uint64(pos)
			}
		default: // an answer
			g.svReset = uint64(pos)
		}
		g.scanned = uint64(pos)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	return g.svReset, nil
}

// count returns the cycles on thread w since the last reset, of w or of
// the supervisor's threads (svReset), and the position of that reset.
func (g *LoopGuard) count(ctx context.Context, w control.ThreadID, svReset uint64) (int, uint64, error) {
	db := g.Core.Store.DB
	from := ID(g.Scope)
	// The last thing a person did in w: the index on (thread, pos) is read
	// backwards, to the first that matches.
	var inW int64
	err := db.QueryRowContext(ctx, `SELECT pos FROM events WHERE thread_id = ?
		AND ((kind IN ('input', 'answer') AND coalesce(json_extract(data, '$.from'), '') != ?)
			OR (kind = 'turn_started' AND json_extract(data, '$.by') = 'terminal'))
		ORDER BY pos DESC LIMIT 1`, int64(w), from).Scan(&inW)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	reset := max(uint64(inW), svReset)

	// The ends of w's turns that the supervisor started, since the reset.
	ends, err := positions(ctx, db, `SELECT pos FROM events WHERE thread_id = ? AND kind = 'turn_ended' AND pos > ?
		AND json_extract(data, '$.from') = ? ORDER BY pos`, int64(w), int64(reset), from)
	if err != nil || len(ends) == 0 {
		return 0, reset, err
	}
	// The wakes since the first of them, by the last position each covers.
	rows, err := db.QueryContext(ctx, `SELECT json_extract(data, '$.key') FROM events WHERE kind = 'turn_started' AND pos > ?
		AND thread_id IN (SELECT id FROM threads WHERE scope = ? AND confined = 1)
		AND substr(json_extract(data, '$.key'), 1, ?) = ?`, int64(ends[0]), g.Scope, len(WakeKeyPrefix), WakeKeyPrefix)
	if err != nil {
		return 0, 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	defer rows.Close()
	var wakes []uint64
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return 0, 0, fmt.Errorf("supervisor: loop guard: %w", err)
		}
		if to, err := strconv.ParseUint(strings.TrimPrefix(key, WakeKeyPrefix), 10, 64); err == nil {
			wakes = append(wakes, to)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	sort.Slice(wakes, func(i, j int) bool { return wakes[i] < wakes[j] })
	// Each end was told by the first wake that covers its position.
	told := map[uint64]bool{}
	for _, p := range ends {
		i := sort.Search(len(wakes), func(i int) bool { return wakes[i] >= p })
		if i < len(wakes) {
			told[wakes[i]] = true
		}
	}
	return len(told), reset, nil
}

func positions(ctx context.Context, db *sql.DB, q string, args ...any) ([]uint64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("supervisor: loop guard: %w", err)
		}
		out = append(out, uint64(p))
	}
	return out, rows.Err()
}

// LoopNotice is the line that tells a person thread w no longer wakes the
// supervisor.
func LoopNotice(w control.ThreadID, cycles int) string {
	return fmt.Sprintf("Thread #%d went back and forth with the supervisor %d times with no person's message in between, so from now on it does not wake the supervisor. A person's message in thread #%d, or here, lets it wake the supervisor again.",
		w, cycles, w)
}

// tell puts the notice for w in the scope's supervisor thread, once for
// that stop: the key names w and the reset before it.
func (g *LoopGuard) tell(ctx context.Context, w control.ThreadID, n int, reset uint64) {
	key := fmt.Sprintf("%s%d-%d", LoopKeyPrefix, w, reset)
	g.mu.Lock()
	done := g.told[key]
	g.mu.Unlock()
	if done {
		return // by this run; the notice's key keeps it once across runs
	}
	sv, _, err := g.Core.LastConfined(ctx, g.Scope)
	if err == nil && sv != 0 && g.Tell != nil {
		err = g.Tell(ctx, sv, key, LoopNotice(w, n))
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			g.Log.Error("supervisor loop guard: telling a person", "scope", g.Scope, "thread", int64(w), "err", err)
		}
		return
	}
	g.mu.Lock()
	if g.told == nil {
		g.told = map[string]bool{}
	}
	g.told[key] = true
	g.mu.Unlock()
	g.Log.Info("supervisor loop guard: a thread no longer wakes the supervisor", "scope", g.Scope, "thread", int64(w), "cycles", n)
}
