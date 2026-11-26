package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Trust from chat (#590): a start blocked at a start-up dialog that named
// files the scope does not trust records them in its state event (trust).
// The owner may let exactly those through for the thread's scope and
// repository; the thread then starts again in a new session in the same
// workspace, and later launches there take them with the config's
// trust_files (TrustedFiles).

// The reasons of the session_replaced event of a thread started again:
// by Trust, and by StartWithoutSettings.
const (
	ReasonTrusted         = string(control.StartTrusted)
	ReasonWithoutSettings = string(control.StartWithoutSettings)
)

// KindStartChosen is the event of a choice for a start that files blocked.
const KindStartChosen control.EventKind = "start_chosen"

// blockedAtStart is the reason of a session that a start-up dialog
// blocked, as its state event records it (agent.BlockedAtStart).
const blockedAtStart = "blocked at start"

func (a *api) Trust(ctx context.Context, id control.ThreadID, blocked int64, files []string) error {
	const name = "Trust"
	scope, err := a.checkThread(ctx, control.OpTrust, name, id)
	if err != nil {
		return err
	}
	if err := a.owner(name, scope); err != nil {
		return err
	}
	if err := a.left(name, scope); err != nil {
		return err
	}
	files = slices.Clone(files)
	slices.Sort(files)
	if len(files) == 0 || slices.Contains(files, "") {
		return fmt.Errorf("core: trust in thread %d: no files: %w", id, session.ErrRefused)
	}
	defer a.c.locks.lock(threadLock(id))()
	var l Launch
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		sid, state, err := a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		named, pos, err := blockedFiles(ctx, tx, id, sid)
		if err != nil {
			return err
		}
		if sid != blocked || state != control.Failed || named == nil || !slices.Equal(named, files) {
			return fmt.Errorf("core: trust in thread %d: its session was not blocked at start by these files: %w", id, session.ErrRefused)
		}
		var repo string
		if err := tx.QueryRowContext(ctx, `SELECT repo FROM threads WHERE id = ?`, int64(id)).Scan(&repo); err != nil {
			return err
		}
		now := a.c.now()
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_files (scope, repo, file, trusted_by, created_at)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, scope, repo, f, string(a.caller.Principal), now.UnixMilli()); err != nil {
				return err
			}
		}
		if l, err = a.c.again(ctx, tx, id, scope, sid, now, ReasonTrusted); err != nil {
			return err
		}
		return a.startChosen(ctx, tx, id, scope, sid, pos, control.StartTrusted, l.Session, now)
	})
	if err != nil {
		return err
	}
	a.c.Log.Info("files trusted", "thread", int64(id), "scope", scope, "files", files, "principal", string(a.caller.Principal))
	return a.c.startAgain(ctx, l)
}

// startAgain hands the runner the session again inserted.
func (c *Core) startAgain(ctx context.Context, l Launch) error {
	if err := c.Runner.Start(ctx, l); err != nil {
		if ferr := c.Machine.Apply(ctx, l.Session, session.Input{Kind: session.Exit, Reason: "start: " + err.Error()}); ferr != nil {
			c.Log.Error("recording a failed start", "session", l.Session, "err", ferr)
		}
		return fmt.Errorf("core: start again: %w", err)
	}
	return nil
}

func (a *api) StartWithoutSettings(ctx context.Context, id control.ThreadID, blocked int64) error {
	const name = "StartWithoutSettings"
	scope, err := a.actThread(ctx, control.OpStart, name, id)
	if err != nil {
		return err
	}
	if err := a.left(name, scope); err != nil {
		return err
	}
	if a.caller.Propose {
		// A caller whose starts are proposals starts nothing itself, and
		// this choice is not one a proposal carries: a person makes it.
		return a.refused(name, scope, "the caller's starts are proposals")
	}
	defer a.c.locks.lock(threadLock(id))()
	var l Launch
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		sid, state, err := a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		named, pos, err := blockedFiles(ctx, tx, id, sid)
		if err != nil {
			return err
		}
		if sid != blocked || state != control.Failed || named == nil {
			return fmt.Errorf("core: start thread %d without settings: its session was not blocked at start by files: %w", id, session.ErrRefused)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET no_project_settings = 1 WHERE id = ?`, int64(id)); err != nil {
			return err
		}
		now := a.c.now()
		if l, err = a.c.again(ctx, tx, id, scope, sid, now, ReasonWithoutSettings); err != nil {
			return err
		}
		return a.startChosen(ctx, tx, id, scope, sid, pos, control.StartWithoutSettings, l.Session, now)
	})
	if err != nil {
		return err
	}
	a.c.Log.Info("thread started without its repository's settings", "thread", int64(id), "scope", scope, "principal", string(a.caller.Principal))
	return a.c.startAgain(ctx, l)
}

// EarlierRunner is a Runner that tells a thread's earlier conversation, as
// a new session that replaces a woken one gets it before its input.
type EarlierRunner interface {
	Earlier(ctx context.Context, q interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}, thread control.ThreadID) (string, error)
}

// owner refuses a caller whose person is not the owner, with one log line.
func (a *api) owner(name, scope string) error {
	if p := a.caller.Principal; p != "" && string(p) == a.c.People.Owner() {
		return nil
	}
	return a.refused(name, scope, "only the owner may trust files")
}

// startChosen records what the caller chose for session sid of thread id,
// which files blocked at its start, as the state event at pos says; started
// is the session started for it, 0 for none.
func (a *api) startChosen(ctx context.Context, tx *store.Tx, id control.ThreadID, scope string, sid int64, pos uint64,
	choice control.StartChoice, started int64, at time.Time) error {
	d := map[string]any{"session": sid, "event": pos, "choice": string(choice)}
	if started != 0 {
		d["new_session"] = started
	}
	if a.caller.Principal != "" {
		d["principal"] = string(a.caller.Principal)
	}
	if a.caller.From != "" {
		d["from"] = a.caller.From
	}
	if a.caller.Via != "" {
		d["via"] = a.caller.Via
	}
	data, _ := json.Marshal(d)
	_, err := tx.Append(ctx, control.Event{Thread: id, Scope: scope, Time: at, Kind: KindStartChosen, Data: data})
	return err
}

// stateChange is a state event of a session.
type stateChange struct {
	pos              uint64
	Session          int64
	From, To, Reason string
	Detail           string
	Trust            []string
}

// rowsQuerier reads rows: the database, or a transaction.
type rowsQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// lastChange returns session sid's last state event that passes ok; nil ok
// takes any. false when there is none.
func lastChange(ctx context.Context, q rowsQuerier, id control.ThreadID, sid int64, ok func(stateChange) bool) (stateChange, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT pos, data FROM events WHERE thread_id = ? AND kind = 'state' ORDER BY pos DESC`, int64(id))
	if err != nil {
		return stateChange{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			d    stateChange
			data string
		)
		if err := rows.Scan(&d.pos, &data); err != nil {
			return stateChange{}, false, err
		}
		if json.Unmarshal([]byte(data), &d) != nil || d.Session != sid || ok != nil && !ok(d) {
			continue
		}
		return d, true, nil
	}
	return stateChange{}, false, rows.Err()
}

// blockedFiles returns, sorted, the files that blocked session sid of
// thread id at its start, and the position of the session's last state
// event, which says so; nil when its last state change was not that.
func blockedFiles(ctx context.Context, q rowsQuerier, id control.ThreadID, sid int64) ([]string, uint64, error) {
	d, ok, err := lastChange(ctx, q, id, sid, nil)
	if err != nil || !ok || d.From != string(control.Creating) || d.To != string(control.Failed) || d.Reason != blockedAtStart || len(d.Trust) == 0 {
		return nil, 0, err
	}
	slices.Sort(d.Trust)
	return slices.Compact(d.Trust), d.pos, nil
}

// failedStart is the thread's current session that failed before its first
// turn, or as it was woken, also once the thread was closed after it; nil
// when it did not.
func (a *api) failedStart(ctx context.Context, t control.ThreadInfo) (*control.FailedStart, error) {
	if t.State != control.Failed && t.State != control.Closed && t.State != control.Removed {
		return nil, nil
	}
	fail := func(err error) (*control.FailedStart, error) {
		return nil, fmt.Errorf("core: thread %d: its start: %w", t.ID, err)
	}
	db := a.c.Store.DB
	var (
		sid   int64
		input string
		ran   bool // the thread had a turn: its start's input was given to its agent
	)
	if err := db.QueryRowContext(ctx, `SELECT s.id, s.first_input,
		EXISTS (SELECT 1 FROM turns u JOIN sessions o ON o.id = u.session_id WHERE o.thread_id = s.thread_id)
		FROM sessions s WHERE s.thread_id = ? ORDER BY s.id DESC LIMIT 1`, int64(t.ID)).Scan(&sid, &input, &ran); err != nil {
		return fail(err)
	}
	// What the thread became after its session ended is not its failure.
	d, ok, err := lastChange(ctx, db, t.ID, sid, func(d stateChange) bool {
		return d.To != string(control.Closed) && d.To != string(control.Removed)
	})
	if err != nil {
		return fail(err)
	}
	if !ok || d.From != string(control.Creating) || d.To != string(control.Failed) {
		return nil, nil
	}
	f := &control.FailedStart{Session: sid, Event: d.pos, Reason: d.Reason}
	if !a.caller.NoText {
		f.Detail, f.Trust = d.Detail, d.Trust
		var first FirstInput
		if !ran && input != "" && json.Unmarshal([]byte(input), &first) == nil {
			f.Text = first.Text
		}
	}
	if a.c.SupervisorOf == nil {
		return f, nil
	}
	var (
		pos  uint64
		data string
	)
	err = db.QueryRowContext(ctx, `SELECT pos, data FROM events WHERE thread_id = ? AND kind = 'thread_created' ORDER BY pos LIMIT 1`, int64(t.ID)).Scan(&pos, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return fail(err)
	}
	var created struct{ From string }
	if json.Unmarshal([]byte(data), &created) == nil && created.From != "" {
		if f.Supervisor, err = a.c.SupervisorOf(ctx, db, t.Scope, created.From, pos); err != nil {
			return fail(err)
		}
	}
	return f, nil
}

// again inserts a new session for thread id, whose session sid failed at
// its start, with sid's agent, model, effort and first input, in the same
// workspace, and returns its launch. Like any started session it is marked
// for launch, so that a daemon that dies before the runner has it starts
// it at its next start (Recover).
func (c *Core) again(ctx context.Context, tx *store.Tx, id control.ThreadID, scope string, sid int64, at time.Time, reason string) (Launch, error) {
	l := Launch{Thread: id}
	var fi string
	if err := tx.QueryRowContext(ctx, `SELECT agent, model, effort, first_input FROM sessions WHERE id = ?`, sid).
		Scan(&l.Agent, &l.Model, &l.Effort, &fi); err != nil {
		return l, err
	}
	var old FirstInput
	if fi != "" {
		json.Unmarshal([]byte(fi), &old)
	}
	var workspace sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT workspace, confined FROM threads WHERE id = ?`, int64(id)).Scan(&workspace, &l.Confined); err != nil {
		return l, err
	}
	dir := old.Dir
	if dir == "" {
		dir = workspace.String
	}
	first := FirstInput{Principal: old.Principal, From: old.From, Key: old.Key, Text: old.Text, Link: old.Link,
		Before: old.Before, After: old.After, Via: old.Via, Files: old.Files, Attachments: old.Attachments, Context: old.Context, Dir: dir, NoInput: old.NoInput}
	if old.Wake {
		// A woken session's agent would have resumed the conversation; the
		// new one gets it before the input, as a session that replaces one
		// that cannot resume does.
		if e, ok := c.Runner.(EarlierRunner); ok {
			earlier, err := e.Earlier(ctx, tx, id)
			if err != nil {
				c.Log.Error("the thread's earlier context", "thread", int64(id), "err", err) // the new session starts without it
			}
			first.Context = earlier
		}
	}
	fb, _ := json.Marshal(first)
	now := at.UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO sessions (thread_id, agent, state, model, effort, first_input, launch, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, int64(id), l.Agent, string(control.Creating), l.Model, l.Effort, string(fb), now, now)
	if err != nil {
		return l, err
	}
	if l.Session, err = res.LastInsertId(); err != nil {
		return l, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ?`,
		string(control.Creating), now, int64(id)); err != nil {
		return l, err
	}
	data, _ := json.Marshal(map[string]any{"session": sid, "new_session": l.Session, "reason": reason})
	if _, err := tx.Append(ctx, control.Event{Thread: id, Scope: scope, Time: at, Kind: "session_replaced", Data: data}); err != nil {
		return l, err
	}
	l.Dir, l.Principal, l.From, l.Key, l.Context, l.NoInput = first.Dir, first.Principal, first.From, first.Key, first.Context, first.NoInput
	l.Input = control.Input{Text: first.Text, Link: first.Link, Before: first.Before, After: first.After, Via: first.Via}
	return l, nil
}

func (a *api) Trusts(ctx context.Context) ([]control.TrustedFile, error) {
	if err := a.check(control.OpRead, "Trusts", ""); err != nil {
		return nil, err
	}
	rows, err := a.c.Store.DB.QueryContext(ctx, `SELECT id, scope, repo, file, trusted_by, created_at FROM trusted_files ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("core: trusts: %w", err)
	}
	defer rows.Close()
	out := []control.TrustedFile{}
	for rows.Next() {
		var (
			t  control.TrustedFile
			by string
			at int64
		)
		if err := rows.Scan(&t.ID, &t.Scope, &t.Repo, &t.File, &by, &at); err != nil {
			return nil, err
		}
		if !a.sees(t.Scope) {
			continue
		}
		t.By, t.At = control.Principal(by), time.UnixMilli(at).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *api) Untrust(ctx context.Context, id int64) error {
	const name = "Untrust"
	if err := a.check(control.OpTrust, name, ""); err != nil {
		return err
	}
	if err := a.owner(name, ""); err != nil {
		return err
	}
	var t control.TrustedFile
	err := a.c.Store.DB.QueryRowContext(ctx, `SELECT scope, repo, file FROM trusted_files WHERE id = ?`, id).Scan(&t.Scope, &t.Repo, &t.File)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("core: trusted file %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if !slices.Contains(a.caller.Scopes, t.Scope) {
		a.hidden(name, "trusted_file", fmt.Sprint(id), t.Scope)
		return fmt.Errorf("core: trusted file %d: %w", id, ErrNotFound)
	}
	if _, err := a.c.Store.DB.ExecContext(ctx, `DELETE FROM trusted_files WHERE id = ?`, id); err != nil {
		return err
	}
	a.c.Log.Info("trust withdrawn", "scope", t.Scope, "repo", t.Repo, "file", t.File, "principal", string(a.caller.Principal))
	return nil
}

// TrustedFiles returns the files Trust recorded for scope's threads on
// repo, for each launch beside the config's trust_files.
func (c *Core) TrustedFiles(ctx context.Context, scope, repo string) ([]string, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT file FROM trusted_files WHERE scope = ? AND repo = ? ORDER BY file`, scope, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
