// Package core implements the control API on top of the store and the
// session state machine. Every call goes through one value bound to a
// Caller, and one function checks it.
package core

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Errors returned by the API. They are wrapped with details.
var (
	ErrForbidden = errors.New("not allowed")
	ErrDisabled  = errors.New("scope is not enabled")
	ErrBusy      = errors.New("a turn is running")
	ErrNotFound  = errors.New("not found")
	ErrNotOpen   = errors.New("the request is not open") // answered, withdrawn, or its agent ended
)

// Scope is what the core needs to know about one scope.
type Scope struct {
	Enabled bool
	Owner   string // the name of the client that gives the scope its threads
	Model   string // default model
	Effort  string // default effort
}

// Workspaces makes the working directory of a new thread.
type Workspaces interface {
	// Make makes the working directory of a new thread. Right before the
	// directory appears at its place, Make calls placed with what will
	// identify it there; when placed fails, the directory does not appear.
	Make(ctx context.Context, thread control.ThreadID, repo string, placed func(Placement) error) (dir string, err error)
	// Adopt returns the directory of p when the one at p.Dir is the
	// directory a Make that called placed with p put there, as when a daemon
	// died before it recorded what Make returned. ok is false when nothing
	// is there yet. Any other directory is refused. Adopt changes nothing.
	Adopt(ctx context.Context, thread control.ThreadID, p Placement) (dir string, ok bool, err error)
	// State reads what the workspace at dir holds, and changes nothing.
	State(ctx context.Context, dir string) (control.WorkspaceState, error)
}

// Placement identifies a directory a Make put in place: where, and the
// device and inode of the directory it moved there.
type Placement struct {
	Dir   string `json:"dir"`
	Dev   uint64 `json:"dev"`
	Inode uint64 `json:"inode"`
}

// Runner runs agent sessions. The core tells it what to do; what the agent
// then reports comes back through the state machine.
type Runner interface {
	// Start starts the agent of a new session. Once the agent reports that it
	// is ready, the runner sends it s.Input as a Send of s.Principal and s.Key.
	Start(ctx context.Context, s Launch) error
	// Deliver gives input to a session's agent. pending is its row in
	// pending_inputs, which the runner removes once it has handed it over.
	Deliver(ctx context.Context, sessionID, pending int64, in control.Input) error
	// Answer gives the answer of an open request to a session's agent.
	// req is the agent's own id of the request; pending is as for Deliver.
	Answer(ctx context.Context, sessionID, pending int64, req string, a control.Answer) error
	// Interrupt stops a session's running turn.
	Interrupt(ctx context.Context, sessionID int64) error
	// Resume starts the agent of an asleep session again with its
	// conversation, and sends it in as a Send of principal and key once it
	// reports that it is ready.
	Resume(ctx context.Context, sessionID int64, principal control.Principal, key string, in control.Input) error
}

// Launch is what Runner.Start needs.
type Launch struct {
	Session   int64
	Thread    control.ThreadID
	Agent     string
	Dir       string
	Model     string
	Effort    string
	Principal control.Principal
	Key       string
	Input     control.Input
	// Context is what the thread said before, for a session that replaces
	// one whose conversation the agent no longer has; empty otherwise.
	Context string

	placed *Placement // a workspace a Make placed before the daemon died
}

// FirstInput is the input a session starts with, as sessions.first_input
// holds it. The core stores it with the state change that starts or wakes
// the session, and sets sessions.launch; the runner clears launch when it
// launches the agent, and keeps the input until a turn reports its id.
type FirstInput struct {
	Principal control.Principal `json:"principal"`
	Key       string            `json:"key"`
	Text      string            `json:"text"`
	Sent      bool              `json:"sent,omitempty"`    // the turn was started for it, and it was handed to the codec
	Written   bool              `json:"written,omitempty"` // it was written to the agent; see agent.Input.Written
	Wake      bool              `json:"wake,omitempty"`    // the session was asleep: its agent resumes
	Dir       string            `json:"dir,omitempty"`     // the workspace, once it is made
	// Context goes before Text when the input is handed to the agent; the
	// input event records Text alone. See Launch.Context.
	Context string `json:"context,omitempty"`
	// Placed is the workspace a Make was about to put in place, until Dir
	// records what it returned.
	Placed *Placement `json:"placed,omitempty"`
}

// pend stores input or an answer for a session's agent until the runner
// has handed it over. req is the agent's id of the request an answer is for.
func pend(ctx context.Context, tx *store.Tx, sid int64, req string, v any) (int64, error) {
	b, _ := json.Marshal(v)
	res, err := tx.ExecContext(ctx, `INSERT INTO pending_inputs (session_id, request, data) VALUES (?, ?, ?)`, sid, req, string(b))
	if err != nil {
		return 0, fmt.Errorf("core: pending input: %w", err)
	}
	return res.LastInsertId()
}

// Core is the core of one instance. It has no operations of its own: use
// As to get an API bound to a caller.
type Core struct {
	Store   *store.Store
	Machine *session.Machine
	Scopes  map[string]Scope              // by scope key
	Owners  map[string]control.ScopeOwner // by client name
	// People resolves a caller's From to a person and says who may act in
	// a scope. Without it no input is allowed.
	People     *people.Directory
	Workspaces Workspaces
	Runner     Runner
	Agent      string // the registered name of the agent new sessions use
	Model      string // default model of a scope without one
	Effort     string // default effort of a scope without one
	Log        *slog.Logger

	locks keyedMutex
}

// keyedMutex serializes work per key.
type keyedMutex struct {
	mu   sync.Mutex
	held map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	users int
}

// lock locks key and returns the function that unlocks it.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.held == nil {
		k.held = map[string]*keyLock{}
	}
	l := k.held[key]
	if l == nil {
		l = &keyLock{}
		k.held[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.held, key)
		}
		k.mu.Unlock()
	}
}

// As returns the API as seen by caller. It is the only way to reach an
// operation. A caller's From is resolved to its person here.
func (c *Core) As(caller control.Caller) control.API {
	if caller.From != "" {
		name, _ := c.People.Resolve(caller.From) // "" for a guest
		caller.Principal = control.Principal(name)
	}
	return &api{c: c, caller: caller}
}

type api struct {
	c      *Core
	caller control.Caller
}

// check is the one authorization function: the caller must be allowed op,
// and scope, when it is not empty, must be one of the caller's. A refusal
// writes one log line.
func (a *api) check(op control.Ops, name, scope string) error {
	if a.caller.May.Has(op) && (scope == "" || slices.Contains(a.caller.Scopes, scope)) {
		return nil
	}
	a.c.Log.Warn("control call refused", "op", name, "principal", string(a.caller.Principal), "scope", scope)
	return fmt.Errorf("core: %s in scope %q: %w", name, scope, ErrForbidden)
}

// act checks a call that gives the agent input, or stops it: check, and
// the caller's person must be allowed in scope. It is the one place that
// decides whose input reaches the agent (design, section 5.1).
func (a *api) act(op control.Ops, name, scope string) error {
	if err := a.check(op, name, scope); err != nil {
		return err
	}
	if a.c.People.May(string(a.caller.Principal), scope) {
		return nil
	}
	reason := "the person may not act in the scope"
	if a.caller.Principal == "" {
		reason = "a guest"
	}
	attrs := []any{"op", name, "scope", scope, "reason", reason}
	if client, id, ok := people.Split(a.caller.From); ok {
		attrs = append(attrs, "client", client, "id", id)
	}
	if a.caller.Principal != "" {
		attrs = append(attrs, "person", string(a.caller.Principal))
	}
	a.c.Log.Warn("control call refused", attrs...)
	return fmt.Errorf("core: %s in scope %q: %w", name, scope, ErrForbidden)
}

// filterScope checks a read with filter f and returns the one scope f
// names, or "" for all of the caller's. A filter that names two different
// scopes matches nothing, and is refused.
func (a *api) filterScope(name string, f control.Filter) (string, error) {
	scope := f.Scope
	if scope == "" {
		scope = f.Thread.Scope
	}
	if f.Scope != "" && f.Thread.Scope != "" && f.Scope != f.Thread.Scope {
		a.c.Log.Warn("control call refused", "op", name, "principal", string(a.caller.Principal), "scope", f.Scope, "thread_scope", f.Thread.Scope)
		return "", fmt.Errorf("core: %s: filter names scopes %q and %q: %w", name, f.Scope, f.Thread.Scope, ErrForbidden)
	}
	return scope, a.check(control.OpRead, name, scope)
}

func (a *api) Threads(ctx context.Context, f control.Filter) ([]control.ThreadInfo, error) {
	scope, err := a.filterScope("Threads", f)
	if err != nil {
		return nil, err
	}
	scopes := a.caller.Scopes
	if scope != "" {
		scopes = []string{scope}
	}
	if len(scopes) == 0 {
		return nil, nil
	}
	q := threadSelect + ` WHERE scope IN (?` + strings.Repeat(",?", len(scopes)-1) + `)`
	var args []any
	for _, s := range scopes {
		args = append(args, s)
	}
	if f.Thread.Key != "" {
		q += ` AND key = ?`
		args = append(args, f.Thread.Key)
	}
	rows, err := a.c.Store.DB.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("core: threads: %w", err)
	}
	defer rows.Close()
	var out []control.ThreadInfo
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // the store has one connection: summary needs it
	for i := range out {
		if err := a.summary(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// summary fills the fields of t that come from its sessions, turns and
// events: the current session's agent, model and effort, the number of
// turns, the background of the last turn_ended event, and the last line of
// output unless the caller may not read text.
func (a *api) summary(ctx context.Context, t *control.ThreadInfo) error {
	db := a.c.Store.DB
	err := db.QueryRowContext(ctx, `SELECT s.agent, s.model, s.effort,
		(SELECT count(*) FROM turns u JOIN sessions v ON v.id = u.session_id WHERE v.thread_id = s.thread_id)
		FROM sessions s WHERE s.thread_id = ? ORDER BY s.id DESC LIMIT 1`, t.Number).Scan(&t.Agent, &t.Model, &t.Effort, &t.Turns)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("core: thread summary: %w", err)
	}
	var data []byte
	err = db.QueryRowContext(ctx, `SELECT data FROM events WHERE scope = ? AND thread = ? AND kind = 'turn_ended'
		ORDER BY pos DESC LIMIT 1`, t.ID.Scope, t.ID.Key).Scan(&data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("core: thread summary: %w", err)
	}
	if err == nil {
		var e struct {
			Background int `json:"background"`
		}
		json.Unmarshal(data, &e)
		t.Background = e.Background
	}
	if !a.caller.NoText {
		if t.Last, err = a.c.lastLine(ctx, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// lastLine returns the last line of the thread's text output that is not
// blank, trimmed and cut to control.LastMax bytes at a character boundary.
// Each text event is a whole message of the agent, so a line never spans
// two events.
func (c *Core) lastLine(ctx context.Context, id control.ThreadID) (string, error) {
	rows, err := c.Store.DB.QueryContext(ctx,
		`SELECT data FROM events WHERE scope = ? AND thread = ? AND kind = 'text' ORDER BY pos DESC`, id.Scope, id.Key)
	if err != nil {
		return "", fmt.Errorf("core: last line: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return "", fmt.Errorf("core: last line: %w", err)
		}
		var e struct {
			Text string `json:"text"`
		}
		json.Unmarshal(data, &e)
		lines := strings.Split(e.Text, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return cut(l, control.LastMax), nil
			}
		}
	}
	return "", rows.Err()
}

// cut cuts s to at most n bytes at a character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

const threadSelect = `SELECT id, scope, key, title, repo, started_by, state, created_at, updated_at FROM threads`

func scanThread(row interface{ Scan(...any) error }) (control.ThreadInfo, error) {
	var (
		t          control.ThreadInfo
		by, state  string
		created, u int64
	)
	if err := row.Scan(&t.Number, &t.ID.Scope, &t.ID.Key, &t.Title, &t.Repo, &by, &state, &created, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, fmt.Errorf("core: thread: %w", ErrNotFound)
		}
		return t, fmt.Errorf("core: thread: %w", err)
	}
	t.StartedBy, t.State = control.Principal(by), control.State(state)
	t.Created, t.Updated = time.UnixMilli(created), time.UnixMilli(u)
	return t, nil
}

func (a *api) Thread(ctx context.Context, id control.ThreadID, recent, events int) (control.ThreadDetail, error) {
	if err := a.check(control.OpRead, "Thread", id.Scope); err != nil {
		return control.ThreadDetail{}, err
	}
	t, err := scanThread(a.c.Store.DB.QueryRowContext(ctx, threadSelect+` WHERE scope = ? AND key = ?`, id.Scope, id.Key))
	if err != nil {
		return control.ThreadDetail{}, err
	}
	if err := a.summary(ctx, &t); err != nil {
		return control.ThreadDetail{}, err
	}
	d := control.ThreadDetail{ThreadInfo: t}
	if t.State == control.Waiting {
		if r, ok, err := a.c.openRequest(ctx, a.c.Store.DB, id); err != nil {
			return d, err
		} else if ok {
			if a.caller.NoText {
				r.Text, r.Choices = "", nil
			}
			d.Requests = []control.Request{r}
		}
	}
	if !a.caller.NoText {
		if d.Recent, err = a.c.recentText(ctx, id, recent); err != nil {
			return d, err
		}
	}
	d.Events, err = a.c.Store.Last(ctx, id, min(events, control.MaxEvents), a.mayRead)
	for i := range d.Events {
		d.Events[i] = a.readable(d.Events[i])
	}
	return d, err
}

// querier is the store's database or one of its transactions.
type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// openRequest returns the open request of the thread's current session.
func (c *Core) openRequest(ctx context.Context, q querier, id control.ThreadID) (control.Request, bool, error) {
	var (
		r        control.Request
		approval bool
		choices  string
	)
	err := q.QueryRowContext(ctx, `
		SELECT r.handle, r.approval, r.text, r.choices FROM requests r
		WHERE r.state = 'open' AND r.session_id = (
			SELECT s.id FROM sessions s JOIN threads t ON t.id = s.thread_id
			WHERE t.scope = ? AND t.key = ? ORDER BY s.id DESC LIMIT 1)
		ORDER BY r.id DESC LIMIT 1`, id.Scope, id.Key).Scan(&r.ID, &approval, &r.Text, &choices)
	if errors.Is(err, sql.ErrNoRows) {
		return control.Request{}, false, nil
	}
	if err != nil {
		return control.Request{}, false, fmt.Errorf("core: request: %w", err)
	}
	if approval {
		r.Kind = control.Approval
	}
	var cs []string
	if err := json.Unmarshal([]byte(choices), &cs); err != nil {
		return control.Request{}, false, fmt.Errorf("core: request: %w", err)
	}
	for _, c := range cs {
		r.Choices = append(r.Choices, control.Choice{Label: c, Token: c})
	}
	return r, true, nil
}

// recentText returns the end of the thread's text output, at most max bytes.
func (c *Core) recentText(ctx context.Context, id control.ThreadID, max int) (string, error) {
	if max <= 0 {
		return "", nil
	}
	rows, err := c.Store.DB.QueryContext(ctx,
		`SELECT data FROM events WHERE scope = ? AND thread = ? AND kind = 'text' ORDER BY pos DESC`, id.Scope, id.Key)
	if err != nil {
		return "", fmt.Errorf("core: recent output: %w", err)
	}
	defer rows.Close()
	var parts []string
	n := 0
	for n < max && rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return "", err
		}
		var e struct {
			Text string `json:"text"`
		}
		json.Unmarshal(data, &e)
		parts = append(parts, e.Text)
		n += len(e.Text)
	}
	slices.Reverse(parts)
	s := strings.Join(parts, "")
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s, rows.Err()
}

// title is the first line of a prompt, cut to 80 bytes at a character
// boundary.
func title(prompt string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	return strings.TrimSpace(cut(line, 80))
}

func (a *api) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	if err := a.act(control.OpStart, "Start", r.Scope); err != nil {
		return control.ThreadID{}, err
	}
	sc, ok := a.c.Scopes[r.Scope]
	if !ok || !sc.Enabled {
		a.c.Log.Warn("start refused", "scope", r.Scope, "reason", "scope is not enabled")
		return control.ThreadID{}, fmt.Errorf("core: start in %q: %w", r.Scope, ErrDisabled)
	}
	// The key is per scope, so a caller cannot reach another scope's thread
	// by reusing its key. Starts with one key run one at a time, so the
	// owner is asked for one thread only.
	inboxKey := "start:" + r.Scope + "\x00" + r.Key
	defer a.c.locks.lock(inboxKey)()
	if id, ok, err := a.c.started(ctx, inboxKey); err != nil || ok {
		return id, err
	}

	id := control.ThreadID{Scope: r.Scope, Key: r.Thread}
	if id.Key == "" {
		owner, ok := a.c.Owners[sc.Owner]
		if !ok {
			return control.ThreadID{}, fmt.Errorf("core: start in %q: no owner %q", r.Scope, sc.Owner)
		}
		key, err := owner.NewThread(ctx, r.Scope, r.Key, title(r.Input.Text))
		if err != nil {
			return control.ThreadID{}, fmt.Errorf("core: start in %q: owner: %w", r.Scope, err)
		}
		id.Key = key
	}
	model := cmp.Or(r.Model, sc.Model, a.c.Model)
	effort := cmp.Or(r.Effort, sc.Effort, a.c.Effort)

	var sessionID int64
	dup := false
	err := a.c.Store.Update(ctx, func(tx *store.Tx) error {
		ref, _ := json.Marshal(id)
		ok, err := tx.Accept(ctx, inboxKey, ref)
		if err != nil || !ok {
			dup = !ok
			return err
		}
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (scope, key, title, repo, started_by, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id.Scope, id.Key, title(r.Input.Text), r.Repo, string(a.caller.Principal), string(control.Creating), now, now)
		if err != nil {
			return fmt.Errorf("core: create thread: %w", err)
		}
		thread, _ := res.LastInsertId()
		fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, Key: r.Key, Text: r.Input.Text})
		res, err = tx.ExecContext(ctx,
			`INSERT INTO sessions (thread_id, agent, state, model, effort, first_input, launch, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			thread, a.c.Agent, string(control.Creating), model, effort, string(fi), now, now)
		if err != nil {
			return fmt.Errorf("core: create session: %w", err)
		}
		sessionID, _ = res.LastInsertId()
		data, _ := json.Marshal(map[string]any{"thread": thread, "session": sessionID, "repo": r.Repo,
			"principal": string(a.caller.Principal), "model": model, "effort": effort})
		_, err = tx.Append(ctx, control.Event{Thread: id, Kind: "thread_created", Data: data})
		return err
	})
	if err != nil {
		return control.ThreadID{}, err
	}
	if dup { // a concurrent call with the same key won
		id, _, err := a.c.started(ctx, inboxKey)
		return id, err
	}

	if err := a.c.launch(ctx, Launch{Session: sessionID, Thread: id, Agent: a.c.Agent,
		Model: model, Effort: effort, Principal: a.caller.Principal, Key: r.Key, Input: r.Input}, r.Repo); err != nil {
		return id, err
	}
	return id, nil
}

// launch makes the workspace of a new session, unless l.Dir names it, and
// starts its agent. A failure is recorded as the session's end.
func (c *Core) launch(ctx context.Context, l Launch, repo string) error {
	var err error
	record := func(f FirstInput) error {
		f.Principal, f.Key, f.Text, f.Context = l.Principal, l.Key, l.Input.Text, l.Context
		fi, _ := json.Marshal(f)
		_, err := c.Store.DB.ExecContext(ctx, `UPDATE sessions SET first_input = ? WHERE id = ?`, string(fi), l.Session)
		return err
	}
	if l.Dir == "" && l.placed != nil {
		// A Make before the daemon died may have put it in place.
		var ok bool
		if l.Dir, ok, err = c.Workspaces.Adopt(ctx, l.Thread, *l.placed); err == nil && ok {
			err = record(FirstInput{Dir: l.Dir})
		}
	}
	if l.Dir == "" && err == nil {
		l.Dir, err = c.Workspaces.Make(ctx, l.Thread, repo, func(p Placement) error {
			return record(FirstInput{Placed: &p})
		})
		if err == nil {
			// A daemon that dies before the agent starts finds it again.
			err = record(FirstInput{Dir: l.Dir})
		}
	}
	if err == nil {
		err = c.Runner.Start(ctx, l)
	}
	if err != nil {
		// The thread exists; its session failed to start.
		if ferr := c.Machine.Apply(ctx, l.Session, session.Input{Kind: session.Exit, Reason: "start: " + err.Error()}); ferr != nil {
			c.Log.Error("recording a failed start", "session", l.Session, "err", ferr)
		}
		return fmt.Errorf("core: start: %w", err)
	}
	return nil
}

// resume starts the agent of a woken session. A failure is recorded as the
// session's end.
func (c *Core) resume(ctx context.Context, sid int64, principal control.Principal, key string, in control.Input) error {
	if err := c.Runner.Resume(ctx, sid, principal, key, in); err != nil {
		if ferr := c.Machine.Apply(ctx, sid, session.Input{Kind: session.Exit, Reason: "resume: " + err.Error()}); ferr != nil {
			c.Log.Error("recording a failed resume", "session", sid, "err", ferr)
		}
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// Recover hands the runner what the core committed and a daemon that died
// did not hand over: the agent of a started or woken session that was
// never launched. Input and answers for running agents are handed over by
// the runner when it reconnects. Call it after the runner has reconnected.
func (c *Core) Recover(ctx context.Context) error {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT s.id, s.agent, s.model, s.effort, s.first_input, t.scope, t.key, t.repo
		FROM sessions s JOIN threads t ON t.id = s.thread_id WHERE s.state = ? AND s.launch IN (1, 2) ORDER BY s.id`, string(control.Creating))
	if err != nil {
		return fmt.Errorf("core: recover: %w", err)
	}
	type todo struct {
		l    Launch
		repo string
		f    FirstInput
	}
	var found []todo
	for rows.Next() {
		var x todo
		var fi string
		if err := rows.Scan(&x.l.Session, &x.l.Agent, &x.l.Model, &x.l.Effort, &fi, &x.l.Thread.Scope, &x.l.Thread.Key, &x.repo); err != nil {
			rows.Close()
			return fmt.Errorf("core: recover: %w", err)
		}
		if err := json.Unmarshal([]byte(fi), &x.f); err != nil {
			c.Log.Error("first input unreadable", "session", x.l.Session, "err", err)
			continue
		}
		found = append(found, x)
	}
	rows.Close()
	for _, x := range found {
		in := control.Input{Text: x.f.Text}
		if x.f.Wake {
			err = c.resume(ctx, x.l.Session, x.f.Principal, x.f.Key, in)
		} else {
			x.l.Dir, x.l.Principal, x.l.Key, x.l.Input, x.l.Context = x.f.Dir, x.f.Principal, x.f.Key, in, x.f.Context
			x.l.placed = x.f.Placed
			err = c.launch(ctx, x.l, x.repo)
		}
		if err != nil {
			c.Log.Error("recovering a session", "session", x.l.Session, "err", err)
		}
	}
	return nil
}

// Workspace reads the state of the thread's workspace. The workspace is the
// one its newest session that has one runs in.
func (a *api) Workspace(ctx context.Context, id control.ThreadID) (control.WorkspaceState, error) {
	if err := a.check(control.OpRead, "Workspace", id.Scope); err != nil {
		return control.WorkspaceState{}, err
	}
	dir, err := a.c.workspaceDir(ctx, id)
	if err != nil {
		return control.WorkspaceState{}, err
	}
	st, err := a.c.Workspaces.State(ctx, dir)
	if err != nil {
		return control.WorkspaceState{}, fmt.Errorf("core: workspace of %v: %w", id, err)
	}
	return st, nil
}

// workspaceDir is the directory of the thread's workspace, as its newest
// session that knows one recorded it: in its first input while it starts,
// in its spec once its agent was launched.
func (c *Core) workspaceDir(ctx context.Context, id control.ThreadID) (string, error) {
	var n int
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM threads WHERE scope = ? AND key = ?`, id.Scope, id.Key).Scan(&n); err != nil {
		return "", fmt.Errorf("core: thread %v: %w", id, err)
	}
	if n == 0 {
		return "", fmt.Errorf("core: thread %v: %w", id, ErrNotFound)
	}
	rows, err := c.Store.DB.QueryContext(ctx, `
		SELECT s.id, s.first_input, s.spec FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE t.scope = ? AND t.key = ? ORDER BY s.id DESC`, id.Scope, id.Key)
	if err != nil {
		return "", fmt.Errorf("core: thread %v: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			sid       int64
			fi, spec  string
			first     FirstInput
			specified struct {
				Spec struct {
					Dir string `json:"dir"`
				} `json:"spec"`
			}
		)
		if err := rows.Scan(&sid, &fi, &spec); err != nil {
			return "", fmt.Errorf("core: thread %v: %w", id, err)
		}
		if fi != "" {
			if err := json.Unmarshal([]byte(fi), &first); err != nil {
				return "", fmt.Errorf("core: session %d: first input: %w", sid, err)
			}
		}
		if first.Dir != "" {
			return first.Dir, nil
		}
		if spec != "" {
			if err := json.Unmarshal([]byte(spec), &specified); err != nil {
				return "", fmt.Errorf("core: session %d: spec: %w", sid, err)
			}
		}
		if specified.Spec.Dir != "" {
			return specified.Spec.Dir, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("core: thread %v: %w", id, err)
	}
	return "", fmt.Errorf("core: thread %v has no workspace yet", id)
}

// started returns the thread a Start key created, if it was used before.
func (c *Core) started(ctx context.Context, inboxKey string) (control.ThreadID, bool, error) {
	var data []byte
	err := c.Store.DB.QueryRowContext(ctx, `SELECT data FROM inbox WHERE key = ?`, inboxKey).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return control.ThreadID{}, false, nil
	}
	if err != nil {
		return control.ThreadID{}, false, fmt.Errorf("core: inbox: %w", err)
	}
	var id control.ThreadID
	if err := json.Unmarshal(data, &id); err != nil {
		return id, false, fmt.Errorf("core: inbox: %w", err)
	}
	return id, true, nil
}

// current returns the thread's current session and its state.
func (c *Core) current(ctx context.Context, q querier, id control.ThreadID) (int64, control.State, error) {
	var (
		sid   int64
		state string
	)
	err := q.QueryRowContext(ctx, `
		SELECT s.id, s.state FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE t.scope = ? AND t.key = ? ORDER BY s.id DESC LIMIT 1`, id.Scope, id.Key).Scan(&sid, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("core: thread %v: %w", id, ErrNotFound)
	}
	if err != nil {
		return 0, "", fmt.Errorf("core: thread %v: %w", id, err)
	}
	return sid, control.State(state), nil
}

func (a *api) Send(ctx context.Context, id control.ThreadID, key string, in control.Input, busy control.Busy) error {
	if err := a.act(control.OpSend, "Send", id.Scope); err != nil {
		return err
	}
	var (
		sid       int64
		pending   int64
		accepted  bool
		interrupt bool
		answer    string // the agent's id of the open question this input answers
		wake      bool   // the session is asleep: its agent starts again
	)
	// The state is read in the write transaction, so it cannot change
	// between the Busy decision and the input.
	err := a.c.Store.Update(ctx, func(tx *store.Tx) error {
		var state control.State
		var err error
		sid, state, err = a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		if key != "" {
			data, _ := json.Marshal(map[string]any{"thread": id, "session": sid, "text": in.Text})
			ok, err := tx.Accept(ctx, "send:"+id.Scope+"\x00"+key, data)
			if err != nil || !ok {
				return err // a repeated key is ignored, whatever Busy says
			}
		}
		if state == control.Running {
			switch busy {
			case control.Refuse:
				return fmt.Errorf("core: send to %v: %w", id, ErrBusy)
			case control.Interrupt:
				interrupt = true
			}
		}
		accepted = true
		if state == control.Asleep {
			wake = true
			if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Wake}); err != nil {
				return err
			}
			fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, Key: key, Text: in.Text, Wake: true})
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ?, launch = 1 WHERE id = ?`, string(fi), sid); err != nil {
				return fmt.Errorf("core: wake: %w", err)
			}
			return nil
		}
		if state == control.Waiting {
			// The input is the answer to the open question.
			var req string
			err := tx.QueryRowContext(ctx, `SELECT request FROM requests WHERE session_id = ? AND state = 'open'
				ORDER BY id DESC LIMIT 1`, sid).Scan(&req)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("core: request: %w", err)
			}
			if err == nil {
				answer = req
				if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Answer, Principal: a.caller.Principal,
					Key: key, Text: in.Text, Request: req}); err != nil {
					return err
				}
				pending, err = pend(ctx, tx, sid, req, control.Answer{Text: in.Text})
				return err
			}
		}
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Send, Principal: a.caller.Principal, Key: key, Text: in.Text}); err != nil {
			return err
		}
		pending, err = pend(ctx, tx, sid, "", in)
		return err
	})
	if err != nil || !accepted {
		return err
	}
	if answer != "" {
		return a.c.Runner.Answer(ctx, sid, pending, answer, control.Answer{Text: in.Text})
	}
	if wake {
		if err := a.c.resume(ctx, sid, a.caller.Principal, key, in); err != nil {
			return fmt.Errorf("core: send to %v: %w", id, err)
		}
		return nil
	}
	if interrupt {
		if err := a.c.Runner.Interrupt(ctx, sid); err != nil {
			return fmt.Errorf("core: send to %v: interrupt: %w", id, err)
		}
	}
	return a.c.Runner.Deliver(ctx, sid, pending, in)
}

func (a *api) Answer(ctx context.Context, req control.RequestID, ans control.Answer) error {
	if err := a.check(control.OpAnswer, "Answer", ""); err != nil {
		return err
	}
	var (
		id       control.ThreadID
		sid      int64
		agentReq string
		state    string
	)
	err := a.c.Store.DB.QueryRowContext(ctx, `
		SELECT t.scope, t.key, r.session_id, r.request, r.state FROM requests r
		JOIN sessions s ON s.id = r.session_id JOIN threads t ON t.id = s.thread_id
		WHERE r.handle = ?`, string(req)).Scan(&id.Scope, &id.Key, &sid, &agentReq, &state)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		a.c.Log.Warn("control call refused", "op", "Answer", "principal", string(a.caller.Principal), "request", string(req), "reason", err)
		return fmt.Errorf("core: answer %q: %w", req, err)
	}
	if err := a.act(control.OpAnswer, "Answer", id.Scope); err != nil {
		return err
	}
	if state != session.RequestOpen {
		return fmt.Errorf("core: answer %q: the request is %s: %w", req, state, ErrNotOpen)
	}
	// The machine refuses it, atomically, if it was closed since.
	var pending int64
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Answer, Principal: a.caller.Principal,
			Key: "answer:" + string(req), Text: ans.Text, Request: agentReq}); err != nil {
			return err
		}
		var err error
		pending, err = pend(ctx, tx, sid, agentReq, ans)
		return err
	})
	if err != nil {
		if errors.Is(err, session.ErrRefused) {
			return fmt.Errorf("core: answer %q: %w: %w", req, ErrNotOpen, err)
		}
		return err
	}
	return a.c.Runner.Answer(ctx, sid, pending, agentReq, ans)
}

func (a *api) Stop(ctx context.Context, id control.ThreadID) error {
	if err := a.act(control.OpStop, "Stop", id.Scope); err != nil {
		return err
	}
	sid, state, err := a.c.current(ctx, a.c.Store.DB, id)
	if err != nil || state != control.Running {
		return err
	}
	return a.c.Runner.Interrupt(ctx, sid)
}

// mayRead reports whether the caller may see e: its scope, and every
// scope its data lists under "scopes" (an event about threads in several
// scopes, such as the end of a login outage).
func (a *api) mayRead(e control.Event) bool {
	if !slices.Contains(a.caller.Scopes, e.Thread.Scope) {
		return false
	}
	var d struct {
		Scopes []string `json:"scopes"`
	}
	if len(e.Data) > 0 {
		json.Unmarshal(e.Data, &d) // data that is not an object names no scopes
	}
	for _, s := range d.Scopes {
		if !slices.Contains(a.caller.Scopes, s) {
			return false
		}
	}
	return true
}

// readable is e as the caller may read it: without its text when the
// caller may not read text.
func (a *api) readable(e control.Event) control.Event {
	if a.caller.NoText {
		return control.WithoutText(e)
	}
	return e
}

func (a *api) Events(ctx context.Context, f control.Filter, after uint64) (<-chan control.Event, error) {
	if _, err := a.filterScope("Events", f); err != nil {
		return nil, err
	}
	in := make(chan control.Event)
	out := make(chan control.Event)
	go func() {
		err := a.c.Store.FollowMark(ctx, f, after, in, func(pos uint64) {
			select {
			case in <- control.Event{Pos: pos, Kind: control.CaughtUp}:
			case <-ctx.Done():
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			a.c.Log.Error("following events", "err", err)
		}
		close(in)
	}()
	go func() {
		defer close(out)
		for e := range in {
			if e.Kind != control.CaughtUp && !a.mayRead(e) {
				continue
			}
			e = a.readable(e)
			select {
			case out <- e:
			case <-ctx.Done():
				for range in { // let Follow see ctx and stop
				}
				return
			}
		}
	}()
	return out, nil
}
