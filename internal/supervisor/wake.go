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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
)

// WakeKeyPrefix starts the key of every Send that wakes a supervisor; the
// rest is the position of the last event the wake covers, a "-", and the
// supervisor's thread it is sent to (WakeKey). A reader of the supervisor's
// events tells a wake from a person's input by it, in turn_started's key.
const WakeKeyPrefix = "hataori-wake-"

// WakeKey is the key of the wake that covers the events up to to, sent to
// the supervisor's thread sv. The core takes a send's key once per scope,
// so the thread is part of it: a wake that a supervisor's thread took
// without reading it, as when its resume failed, goes to the thread that
// replaces it under a key of its own (#443).
func WakeKey(sv control.ThreadID, to uint64) string {
	return WakeKeyPrefix + strconv.FormatUint(to, 10) + "-" + strconv.FormatInt(int64(sv), 10)
}

// ParseWakeKey returns the position a wake's key covers and the thread it
// names; a key from before WakeKey named no thread, and has 0.
func ParseWakeKey(key string) (to uint64, sv control.ThreadID, ok bool) {
	rest, found := strings.CutPrefix(key, WakeKeyPrefix)
	if !found {
		return 0, 0, false
	}
	pos, thread, two := strings.Cut(rest, "-")
	to, err := strconv.ParseUint(pos, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if two {
		n, err := strconv.ParseInt(thread, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		sv = control.ThreadID(n)
	}
	return to, sv, true
}

// keyFor is the key a stored wake is sent with to sv: its own, unless it
// names another thread, which a replacement then takes under its own key.
// A key from before WakeKey names no thread: sentTo tells which thread
// took it, if one did.
func keyFor(stored string, sv control.ThreadID, sentTo func(key string) (control.ThreadID, bool, error)) (string, error) {
	to, thread, ok := ParseWakeKey(stored)
	if !ok || thread == sv {
		return stored, nil
	}
	if thread == 0 {
		took, taken, err := sentTo(stored)
		if err != nil || !taken || took == sv {
			return stored, err
		}
	}
	return WakeKey(sv, to), nil
}

// TurnEnded is the event kind that wakes a supervisor.
const TurnEnded control.EventKind = "turn_ended"

// Ended is a thread's latest turn end since its supervisor was last woken.
type Ended struct {
	Thread control.ThreadID
	Event  control.Event // its last turn_ended event
}

// Gate decides whether a wake happens. The waker's Gates are where the
// loop guard (#274) and the budget (Budget, #273) plug in.
type Gate interface {
	// Admit is given the threads of a wake, by number, before its message
	// is made, and returns those that may wake the supervisor. The events
	// of the others are dropped as read. When it returns none, nothing is
	// sent.
	//
	// A time instead holds the whole wake until then: nothing is dropped,
	// the turn ends wait, those that come meanwhile are folded in, and
	// the wake is made again at that time.
	//
	// It is asked only when the supervisor's thread is idle or asleep, but
	// the thread may still start a turn before the wake is sent; the wake
	// is then made again later, and Admit asked again about the same
	// threads. A count of wakes therefore counts the supervisor's
	// turn_started events whose key starts with WakeKeyPrefix, not calls.
	Admit(ctx context.Context, scope string, threads []Ended) ([]Ended, time.Time, error)
}

// Woken is a Gate that is also told of each wake once the supervisor's
// thread accepted it, with the threads the gates admitted to it. A wake
// that an earlier run of the waker stored, and this one sent again, is not
// told: its threads are not stored.
type Woken interface {
	Woken(ctx context.Context, scope string, threads []Ended) error
}

// Clock is the waker's time: the real clock, or a test's.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the clock a Waker and a Budget use when none is given.
var RealClock Clock = realClock{}

// Waker wakes one scope's supervisor (design, section 7.1). It never polls:
// it reads the scope's events from its stored position, and a turn_ended
// of a thread of the scope other than the supervisor's own opens a quiet
// period. The turn ends until it is over, and those while the supervisor's
// turn runs, are folded into one message, a digest, which it gives the
// supervisor's thread with Send and Refuse. A Send refused as busy keeps
// them for after the supervisor's turn.
//
// A wake is stored before it is sent, and its position once it was
// accepted. A daemon that died in between sends the stored wake again
// under the same key, which the core ignores if it was accepted, so the
// supervisor is woken once for each event, across restarts.
//
// It is a client of the control API inside the daemon, as the owner, and
// keeps its position in the store.
type Waker struct {
	Core       *core.Core
	Scope      string
	Owner      control.Principal // whose Send wakes it: the instance's owner
	Quiet      time.Duration     // the quiet period
	OutputMax  int               // the cap, in bytes, of each thread's last output
	ThreadsMax int               // the most threads a digest shows
	// Gates are asked in order about each wake: the loop guard (#274)
	// comes before the budget, so that threads it drops spend nothing.
	Gates []Gate
	Clock Clock // nil for the real clock
	Log   *slog.Logger
	// Hook, when a test sets it, is called at two steps of a wake:
	// "stored", once it is stored and before it is sent, and "sent", once
	// it was accepted and before its position is stored. An error stops
	// the waker there, as a daemon killed at that moment.
	Hook func(step string) error

	read atomic.Uint64
}

// Read returns the position of the last event the waker read.
func (w *Waker) Read() uint64 { return w.read.Load() }

// errHook wraps an error of the Hook, which ends Run.
type errHook struct{ error }

// Run wakes the supervisor until ctx ends. A failure to read events or
// the store is logged, and reading starts again from the stored position.
func (w *Waker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := w.run(ctx)
		var h errHook
		if errors.As(err, &h) {
			w.Log.Warn("supervisor waker stopped by its hook", "scope", w.Scope, "err", h.error)
			return
		}
		if err != nil && ctx.Err() == nil {
			w.Log.Error("supervisor waker", "scope", w.Scope, "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// wake is a digest being sent: it covers the events up to to.
type wake struct {
	to        uint64
	key, text string
	threads   []Ended // what the gates admitted; nil for a wake stored by an earlier run
}

// waking is one run of the waker, from the stored position.
type waking struct {
	w       *Waker
	api     control.API
	pos     uint64                             // stored: the events up to here are handled
	read    uint64                             // the last event read
	ended   map[control.ThreadID]control.Event // folded: each thread's last turn end
	sending *wake                              // stored, not known to be accepted
	sv      control.ThreadID                   // the supervisor's thread, as last looked up
	own     map[control.ThreadID]bool          // whether a thread is a supervisor's, as asked
	quiet   <-chan time.Time                   // the quiet period, while it runs
	retry   <-chan time.Time                   // a send to try again after an error
	later   <-chan time.Time                   // a step towards the end of a gate's hold
	until   time.Time                          // the end of a gate's hold; zero for none
	held    bool                               // waits for the supervisor's thread to change
}

func (w *Waker) clock() Clock {
	if w.Clock == nil {
		return realClock{}
	}
	return w.Clock
}

func (w *Waker) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &waking{w: w, ended: map[control.ThreadID]control.Event{}, own: map[control.ThreadID]bool{},
		api: w.Core.As(control.Caller{Principal: w.Owner, Scopes: []string{w.Scope}, May: control.OpRead | control.OpSend, NoAnswer: true})}
	if err := r.load(ctx); err != nil {
		return err
	}
	r.read = r.pos
	w.read.Store(r.read)
	sv, _, err := w.Core.LastConfined(ctx, w.Scope)
	if err != nil {
		return err
	}
	r.sv = sv
	evs, err := r.api.Events(ctx, control.Filter{Scope: w.Scope}, r.pos)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		for range evs { // let the subscription end
		}
	}()
	if r.sending != nil { // a wake the last run did not finish
		if err := r.attempt(ctx); err != nil {
			return err
		}
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-evs:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errors.New("supervisor: the events ended")
			}
			err = r.event(ctx, e)
		case <-r.quiet:
			r.quiet = nil
			err = r.attempt(ctx)
		case <-r.retry:
			r.retry = nil
			err = r.attempt(ctx)
		case <-r.later:
			r.later = nil
			if r.hold() {
				continue
			}
			r.until = time.Time{}
			err = r.attempt(ctx)
		}
		if err != nil {
			return err
		}
	}
}

// event folds one event of the scope.
func (r *waking) event(ctx context.Context, e control.Event) error {
	if e.Pos > r.read {
		r.read = e.Pos
		r.w.read.Store(e.Pos)
	}
	if e.Kind == control.CaughtUp {
		// Nothing waits: the events read so far need not be read again.
		if len(r.ended) == 0 && r.sending == nil && r.read > r.pos {
			return r.advance(ctx, r.read)
		}
		return nil
	}
	if e.Thread == 0 {
		return nil
	}
	if e.Pos <= r.pos && e.Kind == TurnEnded {
		// Covered by a wake already: one stored by the last run, sent again
		// and accepted before the subscription reached it.
		return nil
	}
	if e.Kind == TurnEnded {
		// The supervisor's thread may be new since it was looked up.
		sv, _, err := r.w.Core.LastConfined(ctx, r.w.Scope)
		if err != nil {
			return err
		}
		r.sv = sv
	}
	if e.Thread == r.sv {
		// Its own turns never wake it, but end what holds a wake.
		if r.held && r.quiet == nil && r.later == nil && (e.Kind == TurnEnded || e.Kind == "state") {
			return r.attempt(ctx)
		}
		return nil
	}
	if e.Kind != TurnEnded {
		return nil
	}
	// A supervisor's thread of the scope other than the current one, as
	// one a person closed and the daemon replaced, wakes nothing either.
	own, ok := r.own[e.Thread]
	if !ok {
		var err error
		if own, err = r.w.Core.Confined(ctx, e.Thread); err != nil {
			return err
		}
		r.own[e.Thread] = own
	}
	if !Fold(r.ended, e, own) {
		return nil
	}
	// While a gate holds the wake, what comes waits for the hold's end.
	if r.quiet == nil && r.sending == nil && r.later == nil {
		r.quiet = r.w.clock().After(r.w.Quiet)
	}
	return nil
}

// attempt sends the stored wake, or makes one of what is folded and sends
// it, unless the supervisor's thread cannot take it now.
func (r *waking) attempt(ctx context.Context) error {
	if r.sending == nil && len(r.ended) == 0 {
		r.held = false
		return nil
	}
	sv, ready, err := r.ready(ctx)
	if err != nil || !ready {
		return err
	}
	if r.sending == nil {
		w, ok, err := r.prepare(ctx)
		if err != nil || !ok {
			return err
		}
		// Making it read every thread: the supervisor's may have started a
		// turn since, which could also have asked a question by now.
		if again, ready, err := r.ready(ctx); err != nil || !ready || again != sv {
			r.held = true
			return err
		}
		r.sending = w
		if err := r.store(ctx); err != nil {
			return err
		}
		if err := r.hook("stored"); err != nil {
			return err
		}
	}
	key, err := keyFor(r.sending.key, sv, func(key string) (control.ThreadID, bool, error) {
		return r.w.Core.SentTo(ctx, r.w.Scope, key)
	})
	if err != nil {
		return err
	}
	if key != r.sending.key {
		// Stored for a thread that is no longer the supervisor's.
		r.sending.key = key
		if err := r.store(ctx); err != nil {
			return err
		}
	}
	err = r.api.Send(ctx, sv, r.sending.key, control.Input{Text: r.sending.text}, control.Refuse)
	switch {
	case err == nil:
		if err := r.hook("sent"); err != nil {
			return err
		}
		r.w.Log.Info("supervisor woken", "scope", r.w.Scope, "thread", int64(sv), "to", r.sending.to)
		for _, g := range r.w.Gates {
			if t, ok := g.(Woken); ok && r.sending.threads != nil {
				if err := t.Woken(ctx, r.w.Scope, r.sending.threads); err != nil {
					r.w.Log.Error("supervisor waker: a gate on a wake", "scope", r.w.Scope, "err", err)
				}
			}
		}
		to := r.sending.to
		r.sending, r.held = nil, false
		if err := r.advance(ctx, to); err != nil {
			return err
		}
		if len(r.ended) > 0 && r.quiet == nil {
			r.quiet = r.w.clock().After(r.w.Quiet)
		}
		return nil
	case errors.Is(err, core.ErrBusy):
		// Not accepted: a repeated key is ignored before a running turn is
		// refused. The next wake is made again, with what came since.
		r.sending, r.held = nil, true
		return r.store(ctx)
	default:
		r.w.Log.Warn("waking the supervisor; trying again", "scope", r.w.Scope, "thread", int64(sv), "err", err)
		r.retry = r.w.clock().After(max(r.w.Quiet, time.Second))
		return nil
	}
}

func (r *waking) hook(step string) error {
	if r.w.Hook == nil {
		return nil
	}
	if err := r.w.Hook(step); err != nil {
		return errHook{err}
	}
	return nil
}

// ready looks up the supervisor's thread and reports whether it may be
// woken now: idle or asleep. Running, it is not sent to, so that a wake
// never goes in while its turn may ask a question; should the turn start
// and ask one before the Send, the waker's caller (NoAnswer) has the Send
// refused as busy instead of taken as the answer. None yet, starting, waiting, failed or closed, it is not sent
// to either. In each case the wake waits for the thread's next turn end
// or change of state.
func (r *waking) ready(ctx context.Context) (control.ThreadID, bool, error) {
	sv, state, err := r.w.Core.LastConfined(ctx, r.w.Scope)
	if err != nil {
		return 0, false, err
	}
	r.sv = sv
	if state != control.Idle && state != control.Asleep {
		r.held = true
		return sv, false, nil
	}
	return sv, true, nil
}

// prepare makes the wake of what is folded. Without a thread the gate
// admits, the events are handled and nothing is sent.
func (r *waking) prepare(ctx context.Context) (*wake, bool, error) {
	var threads []Ended
	for id, e := range r.ended {
		threads = append(threads, Ended{Thread: id, Event: e})
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].Thread < threads[j].Thread })
	for _, g := range r.w.Gates {
		if len(threads) == 0 {
			break
		}
		var until time.Time
		var err error
		if threads, until, err = g.Admit(ctx, r.w.Scope, threads); err != nil {
			return nil, false, err
		}
		if !until.IsZero() {
			// Held: the turn ends wait for it, with what comes meanwhile.
			if r.later == nil {
				r.until = until
				r.hold()
			}
			return nil, false, nil
		}
	}
	if len(threads) == 0 {
		return nil, false, r.advance(ctx, r.read)
	}
	text, err := DigestOf(threads, func(id control.ThreadID) (control.ThreadDetail, error) {
		return r.api.Thread(ctx, id, r.w.OutputMax+utf8.UTFMax, 0)
	}, r.w.OutputMax, r.w.ThreadsMax)
	if err != nil {
		return nil, false, err
	}
	return &wake{to: r.read, key: WakeKey(r.sv, r.read), text: text, threads: threads}, true, nil
}

// holdStep is the longest a gate's hold waits on one timer: a timer does
// not count the time the host slept, the wall clock does.
const holdStep = time.Minute

// hold waits, a step at a time, for the end of the gate's hold, and
// reports whether it has not come yet by the wall clock.
func (r *waking) hold() bool {
	left := r.until.Sub(r.w.clock().Now())
	if left <= 0 {
		return false
	}
	r.later = r.w.clock().After(min(left, holdStep))
	return true
}

// Fold folds e into ended, each thread's last turn end, and reports
// whether it did: only a turn_ended, of a thread that is not a
// supervisor's (own).
func Fold(ended map[control.ThreadID]control.Event, e control.Event, own bool) bool {
	if e.Kind != TurnEnded || e.Thread == 0 || own {
		return false
	}
	ended[e.Thread] = e
	return true
}

// DigestOf is the digest of threads, in their order, each read with
// thread: the first threadsMax of them, each with its last output cut to
// outputMax bytes, and a line counting the others.
func DigestOf(threads []Ended, thread func(control.ThreadID) (control.ThreadDetail, error), outputMax, threadsMax int) (string, error) {
	var lines []Line
	for i, t := range threads {
		if i == threadsMax {
			break
		}
		d, err := thread(t.Thread)
		if err != nil {
			return "", err
		}
		lines = append(lines, LineOf(t.Event, d, outputMax))
	}
	return Digest(lines, len(threads)-len(lines)), nil
}

// advance stores to as handled, and forgets the turn ends up to it.
func (r *waking) advance(ctx context.Context, to uint64) error {
	for id, e := range r.ended {
		if e.Pos <= to {
			delete(r.ended, id)
		}
	}
	r.pos = max(r.pos, to)
	return r.store(ctx)
}

// load reads the stored position and wake. A scope seen for the first time
// starts after the events already stored: what happened before its
// supervisor was named does not wake it.
func (r *waking) load(ctx context.Context) error {
	db := r.w.Core.Store.DB
	var (
		to         sql.NullInt64
		key, text  sql.NullString
		pos        int64
		scope      = r.w.Scope
		selectWake = `SELECT pos, send_to, send_key, send_text FROM supervisor_wakes WHERE scope = ?`
	)
	err := db.QueryRowContext(ctx, selectWake, scope).Scan(&pos, &to, &key, &text)
	if errors.Is(err, sql.ErrNoRows) {
		if err = Begin(ctx, r.w.Core, scope); err == nil {
			err = db.QueryRowContext(ctx, selectWake, scope).Scan(&pos, &to, &key, &text)
		}
	}
	if err != nil {
		return fmt.Errorf("supervisor: the waker's position: %w", err)
	}
	r.pos = uint64(pos)
	if to.Valid {
		r.sending = &wake{to: uint64(to.Int64), key: key.String, text: text.String}
	}
	return nil
}

// Begin stores where the waker of scope starts reading, the first time it
// is asked: after the events already stored, so that what happened before
// the scope named its supervisor wakes nothing. A scope with a position
// keeps it. The daemon calls it for each such scope before it reconnects
// or recovers anything, so that every event it writes can wake the
// supervisor (#445).
func Begin(ctx context.Context, c *core.Core, scope string) error {
	err := c.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO supervisor_wakes (scope, pos)
			SELECT ?, coalesce(max(pos), 0) FROM events WHERE true ON CONFLICT (scope) DO NOTHING`, scope)
		return err
	})
	if err != nil {
		return fmt.Errorf("supervisor: the waker's position: %w", err)
	}
	return nil
}

// store writes the position and the wake being sent.
func (r *waking) store(ctx context.Context) error {
	var to, key, text any
	if s := r.sending; s != nil {
		to, key, text = int64(s.to), s.key, s.text
	}
	err := r.w.Core.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_wakes SET pos = ?, send_to = ?, send_key = ?, send_text = ? WHERE scope = ?`,
			int64(r.pos), to, key, text, r.w.Scope)
		return err
	})
	if err != nil {
		return fmt.Errorf("supervisor: storing the waker's position: %w", err)
	}
	return nil
}

// Line is one thread of a digest.
type Line struct {
	Thread     control.ThreadID
	Title      string
	Outcome    string // as turn_ended says: completion, blocked or failure
	Reason     string // for a failure
	Background int
	Last       string // the end of its output, at most the cap
	Cut        int    // the cap, when Last was cut to it; 0 when not
}

// LineOf is the digest line of a thread whose last turn end is e, from
// the thread as read: its last output is cut to max bytes, at a character.
func LineOf(e control.Event, d control.ThreadDetail, max int) Line {
	var end struct {
		Outcome    string `json:"outcome"`
		Reason     string `json:"reason"`
		Background int    `json:"background"`
	}
	json.Unmarshal(e.Data, &end) // the store checked the event against its schema
	l := Line{Thread: e.Thread, Title: d.Title, Outcome: end.Outcome, Reason: end.Reason, Background: end.Background,
		Last: strings.TrimRight(d.Recent, "\n")}
	if len(l.Last) > max {
		cut := len(l.Last) - max
		for cut < len(l.Last) && !utf8.RuneStart(l.Last[cut]) {
			cut++
		}
		l.Last, l.Cut = l.Last[cut:], max
	}
	return l
}

// Digest is the message that wakes a supervisor: one paragraph per thread,
// by number, and a line counting more threads that it does not show.
func Digest(lines []Line, more int) string {
	var b strings.Builder
	n := len(lines) + more
	if n == 1 {
		b.WriteString("hataori: a turn ended in 1 thread of this channel.\n")
	} else {
		fmt.Fprintf(&b, "hataori: turns ended in %d threads of this channel.\n", n)
	}
	for _, l := range lines {
		fmt.Fprintf(&b, "\nThread %d", l.Thread)
		if l.Title != "" {
			fmt.Fprintf(&b, ": %s", l.Title)
		}
		b.WriteString("\nEnded: ")
		switch l.Outcome {
		case "completion":
			b.WriteString("completed")
		case "blocked":
			b.WriteString("blocked on a question")
		case "failure":
			b.WriteString("failed")
			if l.Reason != "" {
				fmt.Fprintf(&b, " (%s)", l.Reason)
			}
		default:
			b.WriteString(l.Outcome)
		}
		fmt.Fprintf(&b, ". Background tasks: %d.\n", l.Background)
		switch {
		case l.Last == "":
			b.WriteString("Last output: none.\n")
		case l.Cut > 0:
			fmt.Fprintf(&b, "Last output, cut to its last %d bytes:\n", l.Cut)
		default:
			b.WriteString("Last output:\n")
		}
		if l.Last != "" {
			for _, line := range strings.Split(l.Last, "\n") {
				b.WriteString(strings.TrimRight("> "+line, " ") + "\n")
			}
		}
	}
	switch {
	case more == 1:
		b.WriteString("\nAnd 1 more thread; list_threads and read_thread show it.\n")
	case more > 1:
		fmt.Fprintf(&b, "\nAnd %d more threads; list_threads and read_thread show them.\n", more)
	}
	return b.String()
}
