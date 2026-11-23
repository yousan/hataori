package supervisor

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
//
// A wake that went past a gate's hold gave only some threads' turn ends
// (#677): its key has one more part, those threads, so that a reader of
// the log does not take it for a wake that covers the others'.
func WakeKey(sv control.ThreadID, to uint64, only ...control.ThreadID) string {
	key := WakeKeyPrefix + strconv.FormatUint(to, 10) + "-" + strconv.FormatInt(int64(sv), 10)
	for i, id := range only {
		sep := "."
		if i == 0 {
			sep = "-"
		}
		key += sep + strconv.FormatInt(int64(id), 10)
	}
	return key
}

// WakeOnly returns the threads that a wake's key says are all it gave; nil
// for a wake that covers every turn end up to its position.
func WakeOnly(key string) []control.ThreadID {
	rest, _ := strings.CutPrefix(key, WakeKeyPrefix)
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) < 3 {
		return nil
	}
	var out []control.ThreadID
	for _, p := range strings.Split(parts[2], ".") {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n <= 0 {
			return nil
		}
		out = append(out, control.ThreadID(n))
	}
	return out
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
		var three bool
		if thread, _, three = strings.Cut(thread, "-"); three && WakeOnly(key) == nil {
			return 0, 0, false
		}
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
	return WakeKey(sv, to, WakeOnly(stored)...), nil
}

// TurnEnded is the event kind that wakes a supervisor.
const TurnEnded control.EventKind = "turn_ended"

// The event kinds of a person's decision of a supervisor's request, in a
// supervisor's thread of the scope: of an interrupt request, and of a
// proposal. Each wakes the supervisor too, folded into the next wake as a
// turn end is (design, section 7.1, "Autonomy").
const (
	ApprovalAnswered control.EventKind = "approval_answered"
	ProposalAnswered control.EventKind = "proposal_answered"
)

// Ended is a thread's latest turn end since its supervisor was last woken.
type Ended struct {
	Thread control.ThreadID
	Event  control.Event // its last turn_ended event
	// Stale marks a turn end that ended with background tasks running,
	// after which the thread started no turn for BackgroundWait: it wakes
	// the supervisor once, saying the background work has not come back.
	Stale bool
	// Asked marks a thread the supervisor started for a person, not woken
	// for yet (#677).
	Asked bool
}

// BackgroundWait is how long after a turn end with background tasks
// running the waker waits for the thread's next turn before it wakes the
// supervisor for it (#672).
const BackgroundWait = time.Hour

// Waiting reports whether e, a worker's turn end, waits for the thread's
// background tasks: it completed with some still running. It wakes nobody;
// the thread's next turn end does, or BackgroundWait without one. A turn
// end that is blocked on a question, or failed, wakes as any.
func Waiting(e control.Event) bool {
	var d struct {
		Outcome    string `json:"outcome"`
		Background int    `json:"background"`
	}
	json.Unmarshal(e.Data, &d) // the store checked the event against its schema
	return e.Kind == TurnEnded && d.Outcome == "completion" && d.Background > 0
}

// Gate decides whether a wake happens. The waker's Gates are where the
// loop guard (#274) and the budget (Budget, #273) plug in.
type Gate interface {
	// Admit is given the threads of a wake, by number, before its message
	// is made, and returns those that may wake the supervisor. The events
	// of the others are dropped as read. When it returns none, nothing is
	// sent.
	//
	// With a time, it holds the wake until then: nothing is dropped, the
	// turn ends wait, those that come meanwhile are folded in, and the
	// wake is made again at that time. The threads it returns with the
	// time, if any, go past the hold, in a wake of their own.
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
	// Files, when set, are the user's files of the scope (design, section
	// 7.1, "Two layers"): each wake names those that changed since the
	// session was last given or told them, and after each turn of the
	// supervisor's thread one line there, through Tell, names those that
	// changed since the last.
	Files Files
	// Tell posts a line in the supervisor's chat thread id, once per key,
	// also across restarts: the chat front end's notice.
	Tell func(ctx context.Context, id control.ThreadID, key, text string) error
	// Hook, when a test sets it, is called at the steps of a wake:
	// "stored", once it is stored and before it is sent, and "sent", once
	// it was accepted and before its position is stored; and "files told",
	// once the line naming the files a supervisor's turn changed was
	// posted, before that is recorded. An error stops the waker there, as
	// a daemon killed at that moment.
	Hook func(step string) error
	// Before, when set, is called right before each wake is sent to the
	// supervisor's thread, as for the files copied into its docs/.
	Before func(ctx context.Context, thread control.ThreadID)

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
	threads   []Ended            // what the gates admitted; nil for a wake stored by an earlier run
	files     []Change           // the user's files it names, told of once it is accepted
	stale     []control.ThreadID // the stale turn ends offered to the gates for it
}

// waking is one run of the waker, from the stored position.
type waking struct {
	w       *Waker
	api     control.API
	pos     uint64                             // stored: the events up to here are handled
	read    uint64                             // the last event read
	ended   map[control.ThreadID]control.Event // folded: each thread's last turn end
	decided []control.Event                    // folded: the decisions of its requests, in order
	sending *wake                              // stored, not known to be accepted
	sv      control.ThreadID                   // the supervisor's thread, as last looked up
	own     map[control.ThreadID]bool          // whether a thread is a supervisor's, as asked
	quiet   <-chan time.Time                   // the quiet period, while it runs
	retry   <-chan time.Time                   // a send to try again after an error
	later   <-chan time.Time                   // a step towards the end of a gate's hold
	until   time.Time                          // the end of a gate's hold; zero for none
	held    bool                               // waits for the supervisor's thread to change
	// waiting holds each worker thread whose last turn end waits for its
	// background tasks (Waiting), until the thread starts a turn or
	// BackgroundWait has passed; then stale holds it, folded like a turn
	// end, until a wake offered it. Both are read again from the event log
	// at each run (loadWaiting), so a restart neither loses nor repeats
	// the wake.
	waiting map[control.ThreadID]control.Event
	stale   map[control.ThreadID]control.Event
	asks    map[control.ThreadID]bool // the threads with a turn end that are marked (#677)
	bg      <-chan time.Time          // a step towards the first waiting thread's BackgroundWait
	caught  bool                      // the stored events were read: waits are timed from now on
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
		waiting: map[control.ThreadID]control.Event{}, stale: map[control.ThreadID]control.Event{}, asks: map[control.ThreadID]bool{},
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
	if err := r.loadWaiting(ctx); err != nil {
		return err
	}
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
		case <-r.bg:
			r.bg = nil
			r.background()
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
		// What waits for background tasks waits from now on, by the clock:
		// a turn end read before this may have its next turn further on.
		r.caught = true
		r.background()
		// Nothing waits: the events read so far need not be read again.
		if r.empty() && r.sending == nil && r.read > r.pos {
			return r.advance(ctx, r.read)
		}
		return nil
	}
	if e.Thread == 0 {
		return nil
	}
	if e.Pos <= r.pos && (e.Kind == TurnEnded || e.Kind == ApprovalAnswered || e.Kind == ProposalAnswered) {
		// Covered by a wake already: one stored by the last run, sent again
		// and accepted before the subscription reached it.
		return nil
	}
	if e.Kind == ApprovalAnswered || e.Kind == ProposalAnswered {
		// A person decided one of the supervisor's requests, in a supervisor's
		// thread: it wakes the supervisor, whose own thread it is.
		own, err := r.isOwn(ctx, e.Thread)
		if err != nil || !Decides(e, own) {
			return err
		}
		r.decided = append(r.decided, e)
		if r.quiet == nil && r.sending == nil && r.later == nil {
			r.quiet = r.w.clock().After(r.w.Quiet)
		}
		return nil
	}
	if e.Kind == TurnEnded || e.Kind == "state" && reopened(e) {
		// The supervisor's thread may be new since it was looked up, or an
		// older one a person reopened (core's ReopenThread).
		sv, _, err := r.w.Core.LastConfined(ctx, r.w.Scope)
		if err != nil {
			return err
		}
		r.sv = sv
	}
	if e.Thread == r.sv && e.Kind == TurnEnded && r.w.Files != nil {
		if err := r.turned(ctx, e); err != nil {
			return err
		}
	}
	if e.Thread == r.sv {
		// Its own turns never wake it, but end what holds a wake.
		if r.held && r.quiet == nil && r.free() && (e.Kind == TurnEnded || e.Kind == "state") {
			return r.attempt(ctx)
		}
		return nil
	}
	if e.Kind == "turn_started" || closed(e) {
		// The thread's background work came back, or it went on, or the
		// thread was closed: the turn end it waited from wakes nobody, and
		// its next one wakes as any.
		delete(r.waiting, e.Thread)
		delete(r.stale, e.Thread)
		return nil
	}
	if e.Kind != TurnEnded {
		return nil
	}
	// A supervisor's thread of the scope other than the current one, as
	// one a person closed and the daemon replaced, wakes nothing either.
	own, err := r.isOwn(ctx, e.Thread)
	if err != nil {
		return err
	}
	if !own {
		// Given already, by a wake that went past a hold: the waker's
		// position is still before it.
		if to, err := r.wokenTo(ctx, e.Thread); err != nil || e.Pos <= to {
			return err
		}
		asked, err := r.asked(ctx)
		if err != nil {
			return err
		}
		r.asks[e.Thread] = asked[e.Thread]
	}
	if !own && Waiting(e) {
		// Not the end of anything yet (#672).
		r.waiting[e.Thread] = e
		if r.caught {
			r.background()
		}
		return nil
	}
	if !Fold(r.ended, e, own) {
		return nil
	}
	// While a gate holds the wake, what comes waits for the hold's end,
	// unless a person waits for it.
	if r.quiet == nil && r.sending == nil && (r.later == nil || r.asks[e.Thread]) {
		r.quiet = r.w.clock().After(r.w.Quiet)
	}
	return nil
}

// free reports whether a wake may be made now for something folded: no
// gate holds it, or a marked thread's turn end is there.
func (r *waking) free() bool {
	if r.later == nil {
		return true
	}
	for id, asked := range r.asks {
		_, ended := r.ended[id]
		_, stale := r.stale[id]
		if asked && (ended || stale) {
			return true
		}
	}
	return false
}

// isOwn reports whether thread id is a supervisor's thread, as asked once.
func (r *waking) isOwn(ctx context.Context, id control.ThreadID) (bool, error) {
	if id == r.sv {
		return true, nil
	}
	own, ok := r.own[id]
	if !ok {
		var err error
		if own, err = r.w.Core.Confined(ctx, id); err != nil {
			return false, err
		}
		r.own[id] = own
	}
	return own, nil
}

// empty reports whether nothing is folded.
func (r *waking) empty() bool { return len(r.ended) == 0 && len(r.decided) == 0 && len(r.stale) == 0 }

// closed reports whether e is a thread's change of state to closed.
func closed(e control.Event) bool {
	if e.Kind != "state" {
		return false
	}
	var d struct {
		To string `json:"to"`
	}
	json.Unmarshal(e.Data, &d) // the store checked the event against its schema
	return d.To == string(control.Closed)
}

// staleLine is the digest's line for a stale turn end.
const staleLine = "Its background work has not come back: no turn started in the hour since this turn end."

// staleIn reports whether text, a digest, gives thread id's turn end as
// stale: the paragraph of the thread has the stale line.
func staleIn(text string, id control.ThreadID) bool {
	at := "\nThread " + strconv.FormatInt(int64(id), 10)
	for i := 0; ; {
		j := strings.Index(text[i:], at)
		if j < 0 {
			return false
		}
		start := i + j + len(at)
		if start < len(text) && (text[start] == ':' || text[start] == '\n') {
			para := text[start:]
			if end := strings.Index(para, "\n\n"); end >= 0 {
				para = para[:end]
			}
			return strings.Contains(para, staleLine)
		}
		i = start
	}
}

// background moves each waiting thread whose BackgroundWait has passed to
// stale, opening the quiet period as a turn end does, and sets the timer
// for the next one. A timer does not count the time the host slept, so it
// waits a step at a time, by the wall clock.
func (r *waking) background() {
	now := r.w.clock().Now()
	var next time.Time
	for id, e := range r.waiting {
		due := e.Time.Add(BackgroundWait)
		if due.After(now) {
			if next.IsZero() || due.Before(next) {
				next = due
			}
			continue
		}
		delete(r.waiting, id)
		r.stale[id] = e
		if r.quiet == nil && r.sending == nil && (r.later == nil || r.asks[id]) {
			r.quiet = r.w.clock().After(r.w.Quiet)
		}
	}
	r.bg = nil
	if !next.IsZero() {
		r.bg = r.w.clock().After(min(next.Sub(now), holdStep))
	}
}

// loadWaiting reads from the event log the worker threads of the scope
// whose last turn end waits for background tasks, with no turn started
// since and not closed, from after where the waker began (since) up to
// its stored position: those after it come again with the events. One
// whose BackgroundWait has passed was told if a wake given to a
// supervisor thread of the scope since then, or the wake stored to be
// sent, gives it as stale; any other is stale now.
func (r *waking) loadWaiting(ctx context.Context) error {
	db := r.w.Core.Store.DB
	rows, err := db.QueryContext(ctx, `SELECT e.pos, e.thread_id, e.time, e.data FROM events e JOIN threads t ON t.id = e.thread_id
		WHERE t.scope = ? AND t.confined = 0 AND t.state != ? AND e.kind = 'turn_ended' AND e.pos <= ?
		AND e.pos > (SELECT since FROM supervisor_wakes WHERE scope = ?)
		AND e.pos = (SELECT max(pos) FROM events WHERE thread_id = e.thread_id AND kind = 'turn_ended')
		AND NOT EXISTS (SELECT 1 FROM events s WHERE s.thread_id = e.thread_id AND s.kind = 'turn_started' AND s.pos > e.pos)`,
		r.w.Scope, string(control.Closed), int64(r.pos), r.w.Scope)
	if err != nil {
		return fmt.Errorf("supervisor: threads waiting on background tasks: %w", err)
	}
	var found []control.Event
	for rows.Next() {
		var (
			e    = control.Event{Kind: TurnEnded, Scope: r.w.Scope}
			ms   int64
			data []byte
		)
		if err := rows.Scan(&e.Pos, &e.Thread, &ms, &data); err != nil {
			rows.Close()
			return fmt.Errorf("supervisor: threads waiting on background tasks: %w", err)
		}
		e.Time, e.Data = time.UnixMilli(ms), json.RawMessage(data)
		if Waiting(e) {
			found = append(found, e)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("supervisor: threads waiting on background tasks: %w", err)
	}
	asked, err := r.asked(ctx)
	if err != nil {
		return err
	}
	now := r.w.clock().Now()
	for _, e := range found {
		r.asks[e.Thread] = asked[e.Thread]
		if due := e.Time.Add(BackgroundWait); due.After(now) {
			r.waiting[e.Thread] = e
			continue
		}
		told, err := r.toldStale(ctx, e)
		if err != nil {
			return err
		}
		if !told {
			r.stale[e.Thread] = e
		}
	}
	if len(r.stale) > 0 && r.quiet == nil {
		r.quiet = r.w.clock().After(r.w.Quiet)
	}
	return nil
}

// toldStale reports whether a wake after e, given to a supervisor thread
// of the scope or stored to be sent, gives e's thread as stale.
func (r *waking) toldStale(ctx context.Context, e control.Event) (bool, error) {
	if r.sending != nil && staleIn(r.sending.text, e.Thread) {
		return true, nil
	}
	rows, err := r.w.Core.Store.DB.QueryContext(ctx, `SELECT json_extract(data, '$.text') FROM events WHERE kind = 'input' AND pos > ?
		AND thread_id IN (SELECT id FROM threads WHERE scope = ? AND confined = 1)
		AND substr(json_extract(data, '$.text'), 1, 9) = 'hataori: '`, int64(e.Pos), r.w.Scope)
	if err != nil {
		return false, fmt.Errorf("supervisor: threads waiting on background tasks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var text sql.NullString
		if err := rows.Scan(&text); err != nil {
			return false, fmt.Errorf("supervisor: threads waiting on background tasks: %w", err)
		}
		if staleIn(text.String, e.Thread) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// attempt sends the stored wake, or makes one of what is folded and sends
// it, unless the supervisor's thread cannot take it now.
func (r *waking) attempt(ctx context.Context) error {
	if r.sending == nil && r.empty() {
		r.held = false
		return nil
	}
	sv, ready, err := r.ready(ctx)
	if err != nil || !ready {
		return err
	}
	if r.sending == nil && r.w.Files != nil {
		// A wake's files are seen as of when it is made (Files.Told), so it
		// is made only once the supervisor's last turn end was read, and
		// what that turn changed was named first (turned).
		var last uint64
		err := r.w.Core.Store.DB.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM events WHERE thread_id = ? AND kind = ?`,
			int64(sv), string(TurnEnded)).Scan(&last)
		if err != nil {
			return fmt.Errorf("supervisor: the supervisor's last turn end: %w", err)
		}
		if last > r.read {
			r.held = true
			return nil
		}
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
	if r.w.Before != nil {
		r.w.Before(ctx, sv)
	}
	err = r.api.Send(ctx, sv, r.sending.key, control.Input{Text: r.sending.text}, control.Refuse)
	switch {
	case err == nil:
		if err := r.hook("sent"); err != nil {
			return err
		}
		r.w.Log.Info("supervisor woken", "scope", r.w.Scope, "thread", int64(sv), "to", r.sending.to)
		if len(r.sending.files) > 0 {
			if err := r.w.Files.Told(ctx, r.w.Scope, r.sending.files); err != nil {
				return err
			}
		}
		for _, g := range r.w.Gates {
			if t, ok := g.(Woken); ok && r.sending.threads != nil {
				if err := t.Woken(ctx, r.w.Scope, r.sending.threads); err != nil {
					r.w.Log.Error("supervisor waker: a gate on a wake", "scope", r.w.Scope, "err", err)
				}
			}
		}
		if err := r.delivered(ctx); err != nil {
			return err
		}
		if !r.empty() && r.quiet == nil && r.free() {
			r.quiet = r.w.clock().After(r.w.Quiet)
		}
		return nil
	case errors.Is(err, core.ErrBusy):
		// Not accepted: a repeated key is ignored before a running turn is
		// refused. The next wake is made again, with what came since.
		r.sending, r.held = nil, true
		return r.store(ctx)
	case stoppedByPlugin(err):
		// A plugin's handler stopped this wake (design, section 7.3): the
		// same text would be stopped again, so it is dropped, and the turn
		// ends it named wake no one. A handler that failed is not this
		// case: the wake is tried again until the plugin is back.
		r.w.Log.Warn("supervisor wake stopped by a plugin", "scope", r.w.Scope, "thread", int64(sv), "err", ErrText(err))
		return r.delivered(ctx)
	default:
		r.w.Log.Warn("waking the supervisor; trying again", "scope", r.w.Scope, "thread", int64(sv), "err", ErrText(err))
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
// admits and without a decision, the events are handled and nothing is
// sent. The decisions name no worker's turn end, so a gate drops none of
// them, but a gate's hold holds them too.
func (r *waking) prepare(ctx context.Context) (*wake, bool, error) {
	var threads []Ended
	for id, e := range r.ended {
		if _, ok := r.stale[id]; !ok {
			threads = append(threads, Ended{Thread: id, Event: e})
		}
	}
	var stale []control.ThreadID
	for id, e := range r.stale {
		threads = append(threads, Ended{Thread: id, Event: e, Stale: true})
		stale = append(stale, id)
	}
	asked, err := r.asked(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range threads {
		threads[i].Asked = asked[threads[i].Thread]
	}
	// By number, those a person waits for first, so that ThreadsMax never
	// cuts them off.
	sort.Slice(threads, func(i, j int) bool {
		if a, b := threads[i].Asked, threads[j].Asked; a != b {
			return a
		}
		return threads[i].Thread < threads[j].Thread
	})
	decided := slices.Clone(r.decided)
	past := false // only what goes past a hold
	for _, g := range r.w.Gates {
		if len(threads) == 0 && len(decided) == 0 {
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
			if len(threads) == 0 {
				return nil, false, nil
			}
			past, decided = true, nil
		}
	}
	if past && len(threads) == 0 {
		return nil, false, nil // a later gate dropped them; the held ones wait
	}
	var only []control.ThreadID
	if past {
		stale = nil
		for _, t := range threads {
			only = append(only, t.Thread)
			if t.Stale {
				stale = append(stale, t.Thread)
			}
		}
	}
	if len(threads) == 0 && len(decided) == 0 {
		r.offered(stale) // dropped by a gate, as read
		return nil, false, r.advance(ctx, r.read)
	}
	// Each thread's output after what a digest gave this session of the
	// supervisor, up to the events this wake covers (#673).
	given, err := r.given(ctx)
	if err != nil {
		return nil, false, err
	}
	text, err := DigestOf(threads, decided, func(id control.ThreadID) (control.ThreadDetail, error) {
		d, err := r.api.Thread(ctx, id, 0, 0)
		if err != nil {
			return d, err
		}
		d.Recent, err = r.output(ctx, id, given[id], r.read, r.w.OutputMax+utf8.UTFMax)
		return d, err
	}, func(id control.ThreadID) bool {
		_, ok := given[id]
		return ok
	}, r.w.OutputMax, r.w.ThreadsMax)
	if err != nil {
		return nil, false, err
	}
	w := &wake{to: r.read, key: WakeKey(r.sv, r.read, only...), text: text, threads: threads, stale: stale}
	if r.w.Files != nil {
		changes, err := r.w.Files.Changed(ctx, r.w.Scope, r.sv)
		if err != nil {
			return nil, false, err
		}
		if len(changes) > 0 {
			var names []string
			for _, c := range changes {
				names = append(names, c.Name)
			}
			w.text += "\n" + FilesChanged(names) + "\n"
			w.files = changes
		}
	}
	return w, true, nil
}

// delivered stores the wake that was sent as given, and clears the marks
// of the threads it gave. A wake past a hold handled its own threads' turn
// ends only: the position stays before what a gate holds.
func (r *waking) delivered(ctx context.Context) error {
	w := r.sending
	ids := Named(w.text) // a wake stored by an earlier run
	if w.threads != nil {
		ids = nil
		for _, t := range w.threads {
			ids = append(ids, t.Thread)
		}
	}
	clear, err := r.woke(ctx, ids, w.to)
	if err != nil {
		return err
	}
	r.offered(w.stale)
	r.sending, r.held = nil, false
	only := WakeOnly(w.key)
	if only == nil {
		return r.advance(ctx, w.to, clear)
	}
	for _, id := range only {
		if e, ok := r.ended[id]; ok && e.Pos <= w.to {
			delete(r.ended, id)
		}
	}
	return r.store(ctx, clear)
}

// turned posts the line that names the user's files that changed by the
// end of the supervisor's turn e, once: its key is e's position, so a
// daemon that died before it recorded them as seen posts nothing more. It
// looks at the files as they are when it reads e, which is at once unless
// the daemon was down or behind. A line not posted is an error, and the
// waker reads e again.
func (r *waking) turned(ctx context.Context, e control.Event) error {
	names, now, err := r.w.Files.Turned(ctx, r.w.Scope, e.Thread, e.Pos)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		if r.w.Tell == nil {
			r.w.Log.Info(FilesNotice(names), "scope", r.w.Scope, "thread", int64(e.Thread))
		} else if err := r.w.Tell(ctx, e.Thread, FilesKeyPrefix+strconv.FormatUint(e.Pos, 10), FilesNotice(names)); err != nil {
			return fmt.Errorf("supervisor: the line naming the files a turn changed: %w", err)
		}
		if err := r.hook("files told"); err != nil {
			return err
		}
	}
	return r.w.Files.Seen(ctx, r.w.Scope, e.Pos, now)
}

// given returns, for each worker thread that a wake given to the current
// session of the supervisor's thread named, the last position such a wake
// covered: its output up to there was given. It is read from the event
// log, the wakes' inputs and the keys of the turns they started, so a
// restart neither repeats nor leaves out output.
//
// Only a wake made for the session counts: one that covers the session's
// start, the thread's first event or the event that replaced the session
// before it. A new session, of a new thread or one that replaced a session
// that could not resume, has none: the wake it was first given, made for
// the session before (the old session's wake, or one stored for the
// thread a person closed), gave it no output it had not seen.
func (r *waking) given(ctx context.Context) (map[control.ThreadID]uint64, error) {
	db := r.w.Core.Store.DB
	var session sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(id) FROM sessions WHERE thread_id = ?`, int64(r.sv)).Scan(&session); err != nil {
		return nil, fmt.Errorf("supervisor: the output given: %w", err)
	}
	out := map[control.ThreadID]uint64{}
	if !session.Valid {
		return out, nil
	}
	var start int64
	if err := db.QueryRowContext(ctx, `SELECT coalesce(
		(SELECT max(pos) FROM events WHERE thread_id = ? AND kind = 'session_replaced' AND json_extract(data, '$.new_session') = ?),
		(SELECT min(pos) FROM events WHERE thread_id = ?), 0)`, int64(r.sv), session.Int64, int64(r.sv)).Scan(&start); err != nil {
		return nil, fmt.Errorf("supervisor: the output given: %w", err)
	}
	// The session's wakes, by the turn each started: what it covers.
	covers := map[int64]uint64{}
	rows, err := db.QueryContext(ctx, `SELECT json_extract(data, '$.turn'), json_extract(data, '$.key') FROM events
		WHERE thread_id = ? AND kind = 'turn_started' AND pos >= ? AND json_extract(data, '$.session') = ?
		AND substr(json_extract(data, '$.key'), 1, ?) = ?`, int64(r.sv), start, session.Int64, len(WakeKeyPrefix), WakeKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("supervisor: the output given: %w", err)
	}
	for rows.Next() {
		var (
			turn int64
			key  sql.NullString
		)
		if err := rows.Scan(&turn, &key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("supervisor: the output given: %w", err)
		}
		if to, _, ok := ParseWakeKey(key.String); ok && to >= uint64(start) {
			covers[turn] = to
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("supervisor: the output given: %w", err)
	}
	if len(covers) == 0 {
		return out, nil
	}
	// Their inputs, the digests: the threads each gave a paragraph to.
	rows, err = db.QueryContext(ctx, `SELECT json_extract(data, '$.turn'), json_extract(data, '$.text') FROM events
		WHERE thread_id = ? AND kind = 'input' AND pos >= ? AND json_extract(data, '$.session') = ?
		AND substr(json_extract(data, '$.text'), 1, 9) = 'hataori: '`, int64(r.sv), start, session.Int64)
	if err != nil {
		return nil, fmt.Errorf("supervisor: the output given: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			turn int64
			text sql.NullString
		)
		if err := rows.Scan(&turn, &text); err != nil {
			return nil, fmt.Errorf("supervisor: the output given: %w", err)
		}
		to, ok := covers[turn]
		if !ok {
			continue // not a wake's input
		}
		for _, id := range Named(text.String) {
			out[id] = max(out[id], to)
		}
	}
	return out, rows.Err()
}

// Named returns the threads a digest gives a paragraph to, in order: each
// paragraph starts, after a blank line, with "Thread N" and a colon or the
// end of the line. A title is one line in a digest (Digest).
func Named(digest string) []control.ThreadID {
	var out []control.ThreadID
	lines := strings.Split(digest, "\n")
	for i := 1; i < len(lines); i++ {
		rest, ok := strings.CutPrefix(lines[i], "Thread ")
		if !ok || lines[i-1] != "" {
			continue
		}
		n, _, _ := strings.Cut(rest, ":")
		if id, err := strconv.ParseInt(n, 10, 64); err == nil && id > 0 {
			out = append(out, control.ThreadID(id))
		}
	}
	return out
}

// outputSep goes between two text events of a thread's output, as in the
// control API's recent output (ThreadDetail.Recent).
const outputSep = "\n\n"

// output returns thread id's text output after position from and up to
// to, its text events oldest first with outputSep between them, cut to its
// last max bytes at the start of a character.
func (r *waking) output(ctx context.Context, id control.ThreadID, from, to uint64, max int) (string, error) {
	rows, err := r.w.Core.Store.DB.QueryContext(ctx, `SELECT json_extract(data, '$.text') FROM events
		WHERE thread_id = ? AND kind = 'text' AND pos > ? AND pos <= ? ORDER BY pos DESC`, int64(id), int64(from), int64(to))
	if err != nil {
		return "", fmt.Errorf("supervisor: a thread's output: %w", err)
	}
	defer rows.Close()
	var parts []string
	n := 0
	for n < max && rows.Next() {
		var text sql.NullString
		if err := rows.Scan(&text); err != nil {
			return "", fmt.Errorf("supervisor: a thread's output: %w", err)
		}
		parts = append(parts, text.String)
		n += len(text.String) + len(outputSep)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("supervisor: a thread's output: %w", err)
	}
	slices.Reverse(parts)
	out := strings.Join(parts, outputSep)
	if len(out) > max {
		i := len(out) - max
		for i < len(out) && !utf8.RuneStart(out[i]) {
			i++
		}
		out = out[i:]
	}
	return out, nil
}

// offered forgets the stale turn ends a wake offered to the gates: they
// woke the supervisor, or a gate dropped them.
func (r *waking) offered(ids []control.ThreadID) {
	for _, id := range ids {
		delete(r.stale, id)
	}
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

// Decides reports whether e is a person's decision of one of the
// supervisor's requests that wakes it: an approval_answered or a
// proposal_answered of a supervisor's thread of the scope (own).
func Decides(e control.Event, own bool) bool {
	return own && e.Thread != 0 && (e.Kind == ApprovalAnswered || e.Kind == ProposalAnswered)
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
// thread: the first threadsMax of them, each with its output cut to
// outputMax bytes, and a line counting the others; then the decisions of
// its requests, in their order. The output is the thread's last, or,
// where since holds for the thread, what came since the last digest that
// gave it (Line.Since); since may be nil.
func DigestOf(threads []Ended, decided []control.Event, thread func(control.ThreadID) (control.ThreadDetail, error),
	since func(control.ThreadID) bool, outputMax, threadsMax int) (string, error) {
	var lines []Line
	for i, t := range threads {
		if i == threadsMax {
			break
		}
		d, err := thread(t.Thread)
		if err != nil {
			return "", err
		}
		l := LineOf(t.Event, d, outputMax)
		l.Stale = t.Stale
		l.Since = since != nil && since(t.Thread)
		lines = append(lines, l)
	}
	var ds []Decision
	for _, e := range decided {
		ds = append(ds, DecisionOf(e))
	}
	return Digest(lines, len(threads)-len(lines), ds...), nil
}

// advance stores to as handled, and forgets the turn ends and the
// decisions up to it.
func (r *waking) advance(ctx context.Context, to uint64, also ...func(*store.Tx) error) error {
	for id, e := range r.ended {
		if e.Pos <= to {
			delete(r.ended, id)
		}
	}
	r.decided = slices.DeleteFunc(r.decided, func(e control.Event) bool { return e.Pos <= to })
	r.pos = max(r.pos, to)
	return r.store(ctx, also...)
}

// load reads the stored position and wake. A scope seen for the first time
// starts after the events already stored: what happened before its
// supervisor was named does not wake it.
func (r *waking) load(ctx context.Context) error {
	db := r.w.Core.Store.DB
	var (
		to         sql.NullInt64
		key, text  sql.NullString
		files      sql.NullString
		pos        int64
		scope      = r.w.Scope
		selectWake = `SELECT pos, send_to, send_key, send_text, send_files FROM supervisor_wakes WHERE scope = ?`
	)
	err := db.QueryRowContext(ctx, selectWake, scope).Scan(&pos, &to, &key, &text, &files)
	if errors.Is(err, sql.ErrNoRows) {
		if err = Begin(ctx, r.w.Core, scope); err == nil {
			err = db.QueryRowContext(ctx, selectWake, scope).Scan(&pos, &to, &key, &text, &files)
		}
	}
	if err != nil {
		return fmt.Errorf("supervisor: the waker's position: %w", err)
	}
	r.pos = uint64(pos)
	if to.Valid {
		r.sending = &wake{to: uint64(to.Int64), key: key.String, text: text.String}
		if files.Valid && r.w.Files != nil {
			if err := json.Unmarshal([]byte(files.String), &r.sending.files); err != nil {
				return fmt.Errorf("supervisor: the wake being sent: %w", err)
			}
		}
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
		_, err := tx.ExecContext(ctx, `INSERT INTO supervisor_wakes (scope, pos, since)
			SELECT ?, coalesce(max(pos), 0), coalesce(max(pos), 0) FROM events WHERE true ON CONFLICT (scope) DO NOTHING`, scope)
		return err
	})
	if err != nil {
		return fmt.Errorf("supervisor: the waker's position: %w", err)
	}
	return nil
}

// store writes the position and the wake being sent.
func (r *waking) store(ctx context.Context, also ...func(*store.Tx) error) error {
	var to, key, text, files any
	if s := r.sending; s != nil {
		to, key, text = int64(s.to), s.key, s.text
		if s.files != nil {
			b, _ := json.Marshal(s.files)
			files = string(b)
		}
	}
	err := r.w.Core.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_wakes SET pos = ?, send_to = ?, send_key = ?, send_text = ?, send_files = ? WHERE scope = ?`,
			int64(r.pos), to, key, text, files, r.w.Scope)
		if err != nil {
			return err
		}
		for _, f := range also {
			if err := f(tx); err != nil {
				return err
			}
		}
		// A cleared mark is needed only while its turn end may be read again.
		_, err = tx.ExecContext(ctx, `DELETE FROM supervisor_asked WHERE scope = ? AND woken <= ?`, r.w.Scope, int64(r.pos))
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
	// Stale: the thread started no turn for BackgroundWait after this
	// turn end, whose background tasks have not come back (#672).
	Stale bool
	// Since: Last is the output since the last digest that gave the
	// thread's output to this session of the supervisor, not its last
	// output (#673).
	Since bool
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

// Decision is a person's decision of one of the supervisor's requests: an
// interrupt request, or a proposal.
type Decision struct {
	Proposal bool             // a proposal's; else an interrupt request's
	Number   int64            // the proposal's number
	Action   string           // the proposal's: send or start
	Target   control.ThreadID // the thread it was for; a start's once it ran
	Outcome  string           // as its event says
	Reason   string           // why an approved proposal could not run
	By       string           // the person who decided it
}

// DecisionOf is the decision of e, an approval_answered or a
// proposal_answered event.
func DecisionOf(e control.Event) Decision {
	var d struct {
		Number    int64            `json:"number"`
		Action    string           `json:"action"`
		Target    control.ThreadID `json:"target"`
		Outcome   string           `json:"outcome"`
		Reason    string           `json:"reason"`
		Principal string           `json:"principal"`
	}
	json.Unmarshal(e.Data, &d) // the store checked the event against its schema
	return Decision{Proposal: e.Kind == ProposalAnswered, Number: d.Number, Action: d.Action, Target: d.Target,
		Outcome: d.Outcome, Reason: d.Reason, By: cmp.Or(d.Principal, "someone")}
}

// line is the decision as one line of a digest.
func (d Decision) line() string {
	if !d.Proposal {
		what := fmt.Sprintf("Your request to interrupt thread %d", d.Target)
		switch d.Outcome {
		case "interrupted":
			return fmt.Sprintf("%s: approved by %s; its turn was interrupted, and it got your reason.", what, d.By)
		case "ended":
			return fmt.Sprintf("%s: approved by %s, but its turn had already ended; nothing was interrupted.", what, d.By)
		case "refused":
			return fmt.Sprintf("%s: refused by %s; nothing was interrupted.", what, d.By)
		}
		return fmt.Sprintf("%s: %s, by %s.", what, d.Outcome, d.By)
	}
	what := fmt.Sprintf("Your proposal %d, to start a thread", d.Number)
	not := "not started"
	if d.Action == "send" {
		what, not = fmt.Sprintf("Your proposal %d, to send to thread %d", d.Number, d.Target), "not sent"
	}
	switch d.Outcome {
	case "run":
		if d.Action == "send" {
			return fmt.Sprintf("%s: approved by %s, and sent.", what, d.By)
		}
		return fmt.Sprintf("%s: approved by %s; it started thread %d.", what, d.By, d.Target)
	case "refused":
		return fmt.Sprintf("%s: refused by %s; %s.", what, d.By, not)
	case "busy":
		return fmt.Sprintf("%s: approved by %s, but %s: the thread was running a turn or waiting on a question.", what, d.By, not)
	case "ended":
		return fmt.Sprintf("%s: approved by %s, but %s: the question it answered was no longer open.", what, d.By, not)
	case "closed":
		return fmt.Sprintf("%s: approved by %s, but %s: the thread is closed.", what, d.By, not)
	case "failed":
		return fmt.Sprintf("%s: approved by %s, but %s: %s.", what, d.By, not, cmp.Or(d.Reason, "it failed"))
	}
	return fmt.Sprintf("%s: %s, by %s.", what, d.Outcome, d.By)
}

// oneLine puts a title on one line.
var oneLine = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// Digest is the message that wakes a supervisor: one paragraph per thread,
// by number, and a line counting more threads that it does not show; then
// one line per decision of its requests.
func Digest(lines []Line, more int, decided ...Decision) string {
	var b strings.Builder
	n := len(lines) + more
	var asked string
	switch len(decided) {
	case 0:
	case 1:
		asked = "1 of your requests was decided"
	default:
		asked = fmt.Sprintf("%d of your requests were decided", len(decided))
	}
	switch {
	case n == 0:
		fmt.Fprintf(&b, "hataori: %s.\n", asked)
	case n == 1 && asked == "":
		b.WriteString("hataori: a turn ended in 1 thread of this channel.\n")
	case n == 1:
		fmt.Fprintf(&b, "hataori: a turn ended in 1 thread of this channel, and %s.\n", asked)
	case asked == "":
		fmt.Fprintf(&b, "hataori: turns ended in %d threads of this channel.\n", n)
	default:
		fmt.Fprintf(&b, "hataori: turns ended in %d threads of this channel, and %s.\n", n, asked)
	}
	for _, l := range lines {
		fmt.Fprintf(&b, "\nThread %d", l.Thread)
		if l.Title != "" {
			// One line: a title's own line breaks would read as the
			// digest's lines (Named, staleIn).
			fmt.Fprintf(&b, ": %s", oneLine.Replace(l.Title))
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
		if l.Stale {
			b.WriteString(staleLine + "\n")
		}
		switch {
		case l.Since && l.Last == "":
			b.WriteString("No output since the last digest.\n")
		case l.Since && l.Cut > 0:
			fmt.Fprintf(&b, "Output since the last digest, cut to its last %d bytes:\n", l.Cut)
		case l.Since:
			b.WriteString("Output since the last digest:\n")
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
	if len(decided) > 0 {
		b.WriteString("\nDecided by a person:\n")
		for _, d := range decided {
			b.WriteString(d.line() + "\n")
		}
	}
	return b.String()
}

// reopened reports whether e, a state event, is a reopen's.
func reopened(e control.Event) bool {
	var d struct{ Reason string }
	return json.Unmarshal(e.Data, &d) == nil && d.Reason == "reopen"
}
