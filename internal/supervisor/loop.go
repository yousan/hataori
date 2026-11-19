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
// What a person does resets W's count: an input or an answer in W from
// anyone but the supervisor, or a message or an answer of a person in a
// supervisor thread of the scope (any input but the inputs hataori makes
// there: a wake, and the first message of the thread).
//
// It counts from the event log, so a restart keeps the counts. A thread
// at the count does not wake the supervisor: Admit drops its turn ends.
// A person is told once, in the supervisor's thread, naming it, with the
// wake that makes the last cycle, or with the first turn end dropped if
// that wake was not told. Other threads keep waking the supervisor. It is the waker's first gate, in
// front of the budget, so that what it drops spends nothing.
type LoopGuard struct {
	Core   *core.Core
	Scope  string
	Cycles int // the count; 0 turns the guard off
	// Tell is the budget's: a line in a thread, once for each key.
	Tell func(ctx context.Context, thread control.ThreadID, key, text string) error
	Log  *slog.Logger
}

var _ Gate = (*LoopGuard)(nil)

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
	var kept []Ended
	for _, t := range threads {
		n, reset, err := g.Count(ctx, t.Thread)
		if err != nil {
			return nil, time.Time{}, err
		}
		if n < g.Cycles {
			kept = append(kept, t)
			if n == g.Cycles-1 && fromSupervisor(t.Event, g.Scope) {
				// This wake makes the last cycle.
				g.tell(ctx, t.Thread, g.Cycles, reset)
			}
			continue
		}
		g.tell(ctx, t.Thread, n, reset)
	}
	return kept, time.Time{}, nil
}

// Count returns the cycles on thread w since the last reset, and the
// position of that reset (0 for none).
func (g *LoopGuard) Count(ctx context.Context, w control.ThreadID) (int, uint64, error) {
	db := g.Core.Store.DB
	from := ID(g.Scope)
	var inW, inSv sql.NullInt64
	// A person's input or answer in w: anyone's but the supervisor's.
	err := db.QueryRowContext(ctx, `SELECT max(pos) FROM events WHERE thread_id = ? AND kind IN ('input', 'answer')
		AND coalesce(json_extract(data, '$.from'), '') != ?`, int64(w), from).Scan(&inW)
	if err != nil {
		return 0, 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	// A person's message or answer in a supervisor thread: any input or
	// answer but the first input of a turn hataori started there.
	err = db.QueryRowContext(ctx, `
		WITH sv AS (SELECT id FROM threads WHERE scope = ? AND confined = 1)
		SELECT max(i.pos) FROM events i WHERE i.thread_id IN (SELECT id FROM sv) AND i.kind IN ('input', 'answer')
		AND NOT (i.kind = 'input' AND EXISTS (
			SELECT 1 FROM events s WHERE s.thread_id = i.thread_id AND s.kind = 'turn_started'
			AND json_extract(s.data, '$.session') = json_extract(i.data, '$.session')
			AND json_extract(s.data, '$.turn') = json_extract(i.data, '$.turn')
			AND substr(json_extract(s.data, '$.key'), 1, ?) = ?)
			AND i.pos = (SELECT min(j.pos) FROM events j WHERE j.thread_id = i.thread_id AND j.kind = 'input'
				AND json_extract(j.data, '$.session') = json_extract(i.data, '$.session')
				AND json_extract(j.data, '$.turn') = json_extract(i.data, '$.turn')))`,
		g.Scope, len(startedKeyPrefix), startedKeyPrefix).Scan(&inSv)
	if err != nil {
		return 0, 0, fmt.Errorf("supervisor: loop guard: %w", err)
	}
	reset := uint64(max(inW.Int64, inSv.Int64))

	// The ends of w's turns that the supervisor started, since the reset.
	ends, err := positions(ctx, db, `SELECT pos FROM events WHERE thread_id = ? AND kind = 'turn_ended' AND pos > ?
		AND json_extract(data, '$.from') = ? ORDER BY pos`, int64(w), int64(reset), from)
	if err != nil || len(ends) == 0 {
		return 0, reset, err
	}
	// The wakes since the reset, by the last position each covers.
	rows, err := db.QueryContext(ctx, `SELECT json_extract(data, '$.key') FROM events WHERE kind = 'turn_started' AND pos > ?
		AND thread_id IN (SELECT id FROM threads WHERE scope = ? AND confined = 1)
		AND substr(json_extract(data, '$.key'), 1, ?) = ?`, int64(reset), g.Scope, len(WakeKeyPrefix), WakeKeyPrefix)
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

// fromSupervisor reports whether turn end e is of a turn the supervisor of
// scope started.
func fromSupervisor(e control.Event, scope string) bool {
	var d struct {
		From string `json:"from"`
	}
	json.Unmarshal(e.Data, &d) // the store checked it against the schema
	return d.From == ID(scope)
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
	sv, _, err := g.Core.LastConfined(ctx, g.Scope)
	if err == nil && sv != 0 && g.Tell != nil {
		err = g.Tell(ctx, sv, fmt.Sprintf("%s%d-%d", LoopKeyPrefix, w, reset), LoopNotice(w, n))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		g.Log.Error("supervisor loop guard: telling a person", "scope", g.Scope, "thread", int64(w), "err", err)
		return
	}
	g.Log.Info("supervisor loop guard: a thread no longer wakes the supervisor", "scope", g.Scope, "thread", int64(w), "cycles", n)
}
