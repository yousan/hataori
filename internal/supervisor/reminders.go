package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/store"
)

// The bounds of a reminder (design, section 7.1, "A wake at a time it
// sets").
const (
	RemindMin     = 5 * time.Minute
	RemindMax     = 7 * 24 * time.Hour
	RemindNoteMax = 1024 // bytes
)

// The states of a reminder, in supervisor_reminders.
const (
	reminderOpen      = "open"      // not given yet; with a wake, being given by it
	reminderGiven     = "given"     // in a wake the supervisor's thread took
	reminderCancelled = "cancelled" // by its number
)

// Reminder is one reminder of a scope.
type Reminder struct {
	Number int64
	Due    time.Time
	Note   string
}

// Reminders are one scope's reminders, kept in the store: each wakes the
// scope's supervisor once, at the time it was set for. They are the
// scope's, not a thread's, so a new supervisor thread gets the open ones.
type Reminders struct {
	Core  *core.Core
	Scope string
	Max   int   // the most open ones
	Clock Clock // nil for the real clock
	Log   *slog.Logger

	changed chan struct{} // told, if nothing waits in it yet, that one was set or cancelled
}

// NewReminders returns scope's reminders, at most max open ones.
func NewReminders(c *core.Core, scope string, max int, log *slog.Logger) *Reminders {
	return &Reminders{Core: c, Scope: scope, Max: max, Log: log, changed: make(chan struct{}, 1)}
}

func (m *Reminders) now() time.Time {
	if m.Clock == nil {
		return RealClock.Now()
	}
	return m.Clock.Now()
}

// Remind sets a reminder, or cancels an open one, as control.API.Remind
// says. One that a stored wake carries has come due and is being given: it
// cannot be cancelled, and counts as open until it was, since a wake that
// is dropped gives it back.
func (m *Reminders) Remind(ctx context.Context, r control.RemindRequest) (control.RemindAnswer, error) {
	a, err := m.remind(ctx, r)
	if err != nil {
		m.Log.Warn("control call refused", "op", "Remind", "scope", m.Scope, "reason", err.Error())
		return control.RemindAnswer{}, err
	}
	select {
	case m.changed <- struct{}{}:
	default: // the waker is to look already
	}
	return a, nil
}

func (m *Reminders) remind(ctx context.Context, r control.RemindRequest) (control.RemindAnswer, error) {
	given := 0
	for _, is := range []bool{r.At != nil, r.Delay != 0, r.Cancel != 0} {
		if is {
			given++
		}
	}
	if given != 1 {
		return control.RemindAnswer{}, errors.New("remind: give a time or a delay to set a reminder, or the number of one to cancel, and only one of them")
	}
	if r.Cancel != 0 {
		return m.cancel(ctx, r.Cancel)
	}
	if strings.TrimSpace(r.Note) == "" {
		return control.RemindAnswer{}, errors.New("remind: a reminder needs a note")
	}
	if n := len(r.Note); n > RemindNoteMax {
		return control.RemindAnswer{}, fmt.Errorf("remind: the note is %d bytes; at most %d", n, RemindNoteMax)
	}
	if !utf8.ValidString(r.Note) {
		return control.RemindAnswer{}, errors.New("remind: the note is not UTF-8")
	}
	now := m.now()
	// Seconds first: a delay too large for a duration is past the bound.
	if r.At == nil && (r.Delay < int64(RemindMin/time.Second) || r.Delay > int64(RemindMax/time.Second)) {
		return control.RemindAnswer{}, fmt.Errorf("remind: %d seconds ahead: from 5 minutes to 7 days only", r.Delay)
	}
	due := now.Add(time.Duration(r.Delay) * time.Second)
	if r.At != nil {
		due = *r.At
		if due.Before(now.Add(RemindMin)) || due.After(now.Add(RemindMax)) {
			return control.RemindAnswer{}, fmt.Errorf("remind: %s is not from 5 minutes to 7 days ahead; it is %s now", dueTime(due), dueTime(now))
		}
	}
	// Kept to the millisecond, rounded up: it is never due early.
	ms := due.UnixMilli()
	if time.UnixMilli(ms).Before(due) {
		ms++
	}
	var id int64
	err := m.Core.Store.Update(ctx, func(tx *store.Tx) error {
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor_reminders WHERE scope = ? AND state = ?`,
			m.Scope, reminderOpen).Scan(&open); err != nil {
			return err
		}
		if m.Max == 0 {
			return errors.New("remind: this channel's reminders_max is 0: no reminder is set")
		}
		if open >= m.Max {
			return fmt.Errorf("remind: %d reminders are open already, and reminders_max is %d: cancel one first", open, m.Max)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO supervisor_reminders (scope, due, note, made, state) VALUES (?, ?, ?, ?, ?)`,
			m.Scope, ms, r.Note, now.UnixMilli(), reminderOpen)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		// What is over is kept for the budget, which counts the last hour.
		_, err = tx.ExecContext(ctx, `DELETE FROM supervisor_reminders WHERE scope = ? AND state != ? AND made < ?`,
			m.Scope, reminderOpen, now.Add(-RemindMax-24*time.Hour).UnixMilli())
		return err
	})
	if err != nil {
		return control.RemindAnswer{}, err
	}
	m.Log.Info("supervisor reminder set", "scope", m.Scope, "reminder", id, "at", time.UnixMilli(ms).Format(time.RFC3339))
	return control.RemindAnswer{Number: id, At: time.UnixMilli(ms)}, nil
}

func (m *Reminders) cancel(ctx context.Context, id int64) (control.RemindAnswer, error) {
	var due int64
	err := m.Core.Store.Update(ctx, func(tx *store.Tx) error {
		var wake sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT due, wake FROM supervisor_reminders WHERE scope = ? AND id = ? AND state = ?`,
			m.Scope, id, reminderOpen).Scan(&due, &wake)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("remind: no open reminder %d in this channel", id)
		}
		if err != nil {
			return err
		}
		if wake.Valid {
			return fmt.Errorf("remind: reminder %d has come due and is being given: it cannot be cancelled any more", id)
		}
		_, err = tx.ExecContext(ctx, `UPDATE supervisor_reminders SET state = ? WHERE id = ?`, reminderCancelled, id)
		return err
	})
	if err != nil {
		return control.RemindAnswer{}, err
	}
	m.Log.Info("supervisor reminder cancelled", "scope", m.Scope, "reminder", id)
	return control.RemindAnswer{Number: id, At: time.UnixMilli(due)}, nil
}

// open returns the scope's open reminders that no stored wake carries, by
// when they are due.
func (m *Reminders) open(ctx context.Context) ([]Reminder, error) {
	rows, err := m.Core.Store.DB.QueryContext(ctx, `SELECT id, due, note FROM supervisor_reminders
		WHERE scope = ? AND state = ? AND wake IS NULL ORDER BY due, id`, m.Scope, reminderOpen)
	if err != nil {
		return nil, fmt.Errorf("supervisor: reminders: %w", err)
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var ms int64
		if err := rows.Scan(&r.Number, &ms, &r.Note); err != nil {
			return nil, fmt.Errorf("supervisor: reminders: %w", err)
		}
		r.Due = time.UnixMilli(ms)
		out = append(out, r)
	}
	return out, rows.Err()
}

// remindersSet counts the reminders set from from until to: each is one send of the
// budget, whatever became of it.
func remindersSet(ctx context.Context, c *core.Core, scope string, from, to time.Time) (int, error) {
	var n int
	err := c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM supervisor_reminders WHERE scope = ? AND made >= ? AND made < ?`,
		scope, from.UnixMilli(), to.UnixMilli()).Scan(&n)
	return n, err
}

// API is api, the supervisor's caller, with the scope's reminders.
func (m *Reminders) API(api control.API) control.API { return &reminding{API: api, m: m} }

type reminding struct {
	control.API
	m *Reminders
}

func (a *reminding) Remind(ctx context.Context, r control.RemindRequest) (control.RemindAnswer, error) {
	return a.m.Remind(ctx, r)
}

// remind reads which reminders are due, and sets the timer for the next
// one. A timer does not count the time the host slept, so it waits a step
// at a time, by the wall clock.
func (r *waking) remind(ctx context.Context) error {
	r.due, r.remindAt = false, nil
	if r.w.Reminders == nil {
		return nil
	}
	open, err := r.w.Reminders.open(ctx)
	if err != nil {
		return err
	}
	now := r.w.clock().Now()
	for _, o := range open {
		if o.Due.After(now) {
			r.remindAt = r.w.clock().After(min(o.Due.Sub(now), holdStep))
			break // the first by time
		}
		r.due = true
	}
	return nil
}

// reminded gives what is due, once remind said so: at once, unless a wake
// is being made, sent or held, which then takes it, or the stored events
// are still read.
func (r *waking) reminded(ctx context.Context) error {
	if err := r.remind(ctx); err != nil {
		return err
	}
	if !r.due || !r.caught || r.quiet != nil || r.sending != nil || r.later != nil || r.held {
		return nil
	}
	return r.attempt(ctx)
}

// errReminder is returned in the transaction that stores a wake when one
// of its reminders was cancelled since it was made.
var errReminder = errors.New("a reminder of the wake was cancelled")

// carriesReminders returns what records, in the transaction that stores a
// wake to be sent under key, that it carries reminders: from then on they
// are not open, until the wake is dropped.
func (r *waking) carriesReminders(ctx context.Context, key string, due []Reminder) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		if err := r.dropsReminders(ctx)(tx); err != nil {
			return err
		}
		for _, d := range due {
			res, err := tx.ExecContext(ctx, `UPDATE supervisor_reminders SET wake = ? WHERE scope = ? AND id = ? AND state = ?`,
				key, r.w.Scope, d.Number, reminderOpen)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				return errors.Join(err, errReminder)
			}
		}
		return nil
	}
}

// dropsReminders returns what takes the reminders back from the stored
// wake, as it is made again.
func (r *waking) dropsReminders(ctx context.Context) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_reminders SET wake = NULL WHERE scope = ? AND state = ?`, r.w.Scope, reminderOpen)
		return err
	}
}

// renamesReminders returns what gives the stored wake's reminders its new
// key.
func (r *waking) renamesReminders(ctx context.Context, from, to string) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_reminders SET wake = ? WHERE scope = ? AND wake = ?`, to, r.w.Scope, from)
		return err
	}
}

// clearsReminders returns what records the reminders that the wake under
// key carries as given, in the transaction that stores it as given.
func (r *waking) clearsReminders(ctx context.Context, key string) func(*store.Tx) error {
	return func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_reminders SET state = ? WHERE scope = ? AND wake = ? AND state = ?`,
			reminderGiven, r.w.Scope, key, reminderOpen)
		return err
	}
}

// ReminderKey is the key of a wake that gives reminders alone, sent to the
// supervisor's thread sv: no turn end waited for it, so a reader of the log
// does not take it for a wake that covers one. A reminder is never given
// under two keys of one thread.
func ReminderKey(sv control.ThreadID, to uint64, numbers ...int64) string {
	key := WakeKey(sv, to) + "-"
	for i, n := range numbers {
		if i > 0 {
			key += "."
		}
		key += "r" + strconv.FormatInt(n, 10)
	}
	return key
}

// WakeReminders returns the reminders that a wake's key says are all it
// gave; nil for any other wake.
func WakeReminders(key string) []int64 {
	rest, _ := strings.CutPrefix(key, WakeKeyPrefix)
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) < 3 {
		return nil
	}
	var out []int64
	for _, p := range strings.Split(parts[2], ".") {
		num, ok := strings.CutPrefix(p, "r")
		n, err := strconv.ParseInt(num, 10, 64)
		if !ok || err != nil || n <= 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// dueTime is a time as a person and the supervisor read it.
func dueTime(t time.Time) string { return t.Format("2006-01-02 15:04 MST") }

// late says how late a reminder is.
func late(d time.Duration) string {
	if d < time.Minute {
		return "on time"
	}
	count := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	days, hours, minutes := int64(d/(24*time.Hour)), int64(d/time.Hour)%24, int64(d/time.Minute)%60
	var parts []string
	if days > 0 {
		parts = append(parts, count(days, "day"))
	}
	if hours > 0 {
		parts = append(parts, count(hours, "hour"))
	}
	if minutes > 0 {
		parts = append(parts, count(minutes, "minute"))
	}
	return strings.Join(parts, " ") + " late"
}

// DueReminders is the part of a wake that gives the reminders due, at now:
// each with its number, its time, how late it is and its note, quoted line
// by line.
func DueReminders(due []Reminder, now time.Time) string {
	var b strings.Builder
	for _, d := range due {
		fmt.Fprintf(&b, "\nReminder %d, due %s, %s:\n", d.Number, dueTime(d.Due), late(now.Sub(d.Due)))
		for _, line := range strings.Split(strings.TrimRight(noteLines.Replace(d.Note), "\n"), "\n") {
			b.WriteString(strings.TrimRight("> "+line, " ") + "\n")
		}
	}
	return b.String()
}

// noteLines makes every line break of a note one that its quoting splits
// at.
var noteLines = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// OpenReminders is the end of a wake: the scope's open reminders, one line
// each, with its number, its time and its note; nothing when none is open.
func OpenReminders(open []Reminder) string {
	if len(open) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nOpen reminders:\n")
	for _, o := range open {
		fmt.Fprintf(&b, "- %d, due %s: %s\n", o.Number, dueTime(o.Due), strings.Join(strings.Fields(noteLines.Replace(o.Note)), " "))
	}
	return b.String()
}

// RemindersNamed returns the reminders a wake gives, in order.
func RemindersNamed(wake string) []int64 {
	var out []int64
	lines := strings.Split(wake, "\n")
	for i := 1; i < len(lines); i++ {
		rest, ok := strings.CutPrefix(lines[i], "Reminder ")
		if !ok || lines[i-1] != "" {
			continue
		}
		num, _, _ := strings.Cut(rest, ",")
		if n, err := strconv.ParseInt(num, 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}
