package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// Limits are a scope's supervisor budget (design, section 7.1). A zero
// limit is off.
type Limits struct {
	Turns  int   // supervisor turns per hour
	Sends  int   // sends and starts of the supervisor's caller per hour
	Tokens int64 // the supervisor's tokens per day
}

// Used is what a scope's supervisor used in the current windows.
type Used struct {
	Hour   time.Time // the start of the current hour, in the zone
	Day    time.Time // the start of the current day, in the zone
	Turns  int       // turn_started events of the scope's supervisor threads this hour
	Sends  int       // sends and starts of the supervisor's caller this hour
	Tokens int64     // tokens of the scope's supervisor threads today
}

// Budget bounds what one scope's supervisor costs. It counts from the
// event log, so a restart keeps the counts:
//
//   - turns: the turn_started events of the scope's supervisor threads in
//     the current hour, whoever started the turn;
//   - sends and starts: the input and answer events whose from is the
//     supervisor's id, in the current hour, each send's and each start's
//     (its first input); and a thread_created of its own whose thread has
//     no input of its yet, as a start that failed before its first input;
//   - tokens: from the usage events of the scope's supervisor threads, by
//     the rule of a thread's Tokens.Today: each report adds what it raises
//     its turn's total by, counted in the day it came.
//
// Hours and days are those of the clock in Zone. Used up is any limit
// reached; then the waker is held until the windows of every limit
// reached have ended (Admit), and a person is told once per such end, in
// the supervisor's thread. When the sends limit is reached, sends and
// starts of the supervisor's caller are refused at the call (API).
type Budget struct {
	Core   *core.Core
	Scope  string
	Limits Limits
	Clock  Clock          // nil for the real clock
	Zone   *time.Location // nil for the local zone
	// Tell puts a line in a thread where people read it, at most once for
	// each key, also across restarts: the daemon gives the chat front end's
	// Notice. Nil logs the line only.
	Tell func(ctx context.Context, thread control.ThreadID, key, text string) error
	Log  *slog.Logger

	mu      sync.Mutex // held while a send or start is counted and made
	granted int        // sends and starts allowed in hour, which the log may not show yet
	hour    time.Time
}

var _ Gate = (*Budget)(nil)

func (b *Budget) now() time.Time {
	c := b.Clock
	if c == nil {
		c = RealClock
	}
	z := b.Zone
	if z == nil {
		z = time.Local
	}
	return c.Now().In(z)
}

// windows returns the starts of the current hour and day.
func windows(now time.Time) (hour, day time.Time) {
	y, m, d := now.Date()
	return time.Date(y, m, d, now.Hour(), 0, 0, 0, now.Location()), time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}

// Used counts what the supervisor used in the current windows.
func (b *Budget) Used(ctx context.Context) (Used, error) {
	var u Used
	u.Hour, u.Day = windows(b.now())
	db := b.Core.Store.DB
	hourMs, nextHourMs := u.Hour.UnixMilli(), u.Hour.Add(time.Hour).UnixMilli()
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind = 'turn_started' AND time >= ? AND time < ?
		AND thread_id IN (SELECT id FROM threads WHERE scope = ? AND confined = 1)`, hourMs, nextHourMs, b.Scope).Scan(&u.Turns)
	if err != nil {
		return u, fmt.Errorf("supervisor: budget: turns: %w", err)
	}
	from := ID(b.Scope)
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM events e WHERE e.scope = ? AND e.time >= ? AND e.time < ?
		AND json_extract(e.data, '$.from') = ? AND (e.kind IN ('input', 'answer')
			OR (e.kind = 'thread_created' AND NOT EXISTS (SELECT 1 FROM events f WHERE f.thread_id = e.thread_id
				AND f.kind = 'input' AND json_extract(f.data, '$.from') = ?)))`,
		b.Scope, hourMs, nextHourMs, from, from).Scan(&u.Sends)
	if err != nil {
		return u, fmt.Errorf("supervisor: budget: sends: %w", err)
	}
	if u.Tokens, err = b.tokens(ctx, u.Day); err != nil {
		return u, err
	}
	b.mu.Lock() // not held by a caller of Used: take is the only one, and counts itself
	if b.hour.Equal(u.Hour) {
		u.Sends = max(u.Sends, b.granted)
	}
	b.mu.Unlock()
	return u, nil
}

// tokens counts the tokens of the scope's supervisor threads in the day
// that starts at day: the usage events of each turn that reported in it,
// in order, each adding what raises the turn's highest total so far, when
// it came in that day.
func (b *Budget) tokens(ctx context.Context, day time.Time) (int64, error) {
	dayMs := day.UnixMilli()
	nextMs := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location()).UnixMilli()
	rows, err := b.Core.Store.DB.QueryContext(ctx, `
		WITH sv AS (SELECT id FROM threads WHERE scope = ? AND confined = 1),
		u AS (SELECT pos, time, data, json_extract(data, '$.turn') AS turn FROM events
			WHERE kind = 'usage' AND thread_id IN (SELECT id FROM sv))
		SELECT time, turn, data FROM u WHERE turn IS NOT NULL AND turn IN (SELECT turn FROM u WHERE time >= ? AND time < ?)
		ORDER BY pos`, b.Scope, dayMs, nextMs)
	if err != nil {
		return 0, fmt.Errorf("supervisor: budget: tokens: %w", err)
	}
	defer rows.Close()
	highest := map[int64]int64{}
	var sum int64
	for rows.Next() {
		var (
			ms, turn int64
			data     []byte
		)
		if err := rows.Scan(&ms, &turn, &data); err != nil {
			return 0, fmt.Errorf("supervisor: budget: tokens: %w", err)
		}
		var c struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
		}
		json.Unmarshal(data, &c) // the store checked it against the schema
		total := c.Input + c.Output + c.CacheRead + c.CacheWrite
		before := highest[turn]
		if total > before {
			highest[turn] = total
			if ms >= dayMs && ms < nextMs {
				sum += total - before
			}
		}
	}
	return sum, rows.Err()
}

// over returns what of u reaches the limits, and when the last of their
// windows ends; nothing and the zero time when none does.
func (l Limits) over(u Used) ([]string, time.Time) {
	var what []string
	var until time.Time
	end := func(t time.Time) {
		if t.After(until) {
			until = t
		}
	}
	if l.Turns > 0 && u.Turns >= l.Turns {
		what = append(what, fmt.Sprintf("%d of %d turns this hour", u.Turns, l.Turns))
		end(u.Hour.Add(time.Hour))
	}
	if l.Sends > 0 && u.Sends >= l.Sends {
		what = append(what, fmt.Sprintf("%d of %d sends and starts this hour", u.Sends, l.Sends))
		end(u.Hour.Add(time.Hour))
	}
	if l.Tokens > 0 && u.Tokens >= l.Tokens {
		what = append(what, fmt.Sprintf("%d of %d tokens today", u.Tokens, l.Tokens))
		end(time.Date(u.Day.Year(), u.Day.Month(), u.Day.Day()+1, 0, 0, 0, 0, u.Day.Location()))
	}
	return what, until
}

// Admit holds a wake while the budget is used up, until the windows of
// every limit reached have ended, and tells a person so, once for that
// end. It is the budget's Gate.
func (b *Budget) Admit(ctx context.Context, scope string, threads []Ended) ([]Ended, time.Time, error) {
	u, err := b.Used(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	what, until := b.Limits.over(u)
	if until.IsZero() {
		return threads, time.Time{}, nil
	}
	b.tell(ctx, u, what, until)
	return nil, until, nil
}

// NoticeKeyPrefix starts the key of a budget notice; the rest is the end
// of the windows it is about, in RFC 3339 and UTC.
const NoticeKeyPrefix = "hataori-budget-"

// Notice is the line that tells a person the budget is used up: what
// reached its limit, and until when the supervisor is not woken; with
// refused, that its sends and starts are refused until refusedUntil.
func Notice(what []string, until time.Time, refused bool, refusedUntil time.Time) string {
	s := fmt.Sprintf("The supervisor's budget for this channel is used up: %s. It is not woken until %s; what happens in the channel until then is told to it then.",
		strings.Join(what, ", "), until.Format("2006-01-02 15:04 MST"))
	if refused {
		s += fmt.Sprintf(" Its sends and starts are refused until %s.", refusedUntil.Format("2006-01-02 15:04 MST"))
	}
	return s
}

// tell puts the notice of u in the scope's supervisor thread, once for
// until, the end of its hold.
func (b *Budget) tell(ctx context.Context, u Used, what []string, until time.Time) {
	refused := b.Limits.Sends > 0 && u.Sends >= b.Limits.Sends
	text := Notice(what, until, refused, u.Hour.Add(time.Hour))
	sv, _, err := b.Core.LastConfined(ctx, b.Scope)
	if err == nil && sv != 0 && b.Tell != nil {
		err = b.Tell(ctx, sv, NoticeKeyPrefix+until.UTC().Format(time.RFC3339), text)
	}
	if err != nil {
		b.Log.Error("supervisor budget: telling a person", "scope", b.Scope, "err", err)
		return
	}
	b.Log.Info("supervisor budget used up", "scope", b.Scope, "used", strings.Join(what, ", "), "until", until.Format(time.RFC3339))
}

// API is api, the supervisor's caller, with its sends and starts counted:
// once the sends limit is reached, they are refused at the call with
// control.ErrForbidden, which says why and until when.
func (b *Budget) API(api control.API) control.API {
	if b.Limits.Sends <= 0 {
		return api
	}
	return &limited{API: api, b: b}
}

type limited struct {
	control.API
	b *Budget
}

func (l *limited) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	var id control.ThreadID
	err := l.b.take(ctx, func() error {
		var err error
		id, err = l.API.Start(ctx, r)
		return err
	})
	return id, err
}

func (l *limited) Send(ctx context.Context, id control.ThreadID, key string, in control.Input, busy control.Busy) error {
	return l.b.take(ctx, func() error { return l.API.Send(ctx, id, key, in, busy) })
}

// take makes call, a send or a start, unless the sends limit is reached.
// Calls are made one at a time, so that two cannot both take the last one.
func (b *Budget) take(ctx context.Context, call func() error) error {
	u, err := b.Used(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hour.Equal(u.Hour) {
		b.hour, b.granted = u.Hour, 0
	}
	u.Sends = max(u.Sends, b.granted)
	if u.Sends >= b.Limits.Sends {
		what, until := b.Limits.over(u)
		b.tell(ctx, u, what, until)
		b.Log.Warn("control call refused", "op", "Send or Start", "scope", b.Scope, "reason", "over budget")
		return fmt.Errorf("over budget: %d of %d sends and starts this hour; none until %s: %w",
			u.Sends, b.Limits.Sends, u.Hour.Add(time.Hour).Format("15:04 MST"), control.ErrForbidden)
	}
	if err := call(); err != nil {
		return err
	}
	b.granted = u.Sends + 1
	return nil
}
