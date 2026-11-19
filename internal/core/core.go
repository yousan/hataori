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
	ErrForbidden = control.ErrForbidden
	ErrDisabled  = errors.New("scope is not enabled")
	ErrBusy      = errors.New("a turn is running")
	ErrNotFound  = errors.New("not found")
	ErrNotOpen   = errors.New("the request is not open") // answered, withdrawn, or its agent ended
	// ErrNoTerminal: the thread's agent runs without a terminal, so there
	// is nothing to attach to.
	ErrNoTerminal = errors.New("the agent has no terminal to attach to")
	// ErrClosed: the thread is closed or removed (control.ErrClosed).
	ErrClosed = control.ErrClosed
	// ErrRemovalRefused is control.ErrRemovalRefused.
	ErrRemovalRefused = control.ErrRemovalRefused
)

// Scope is what the core needs to know about one scope.
type Scope struct {
	Name    string // the config's name for it; wins over its channel's name
	Enabled bool
	Owner   string // the name of the client that gives the scope its threads
	Model   string // default model
	Effort  string // default effort
	// ShowIn lists the "<client>:<channel>" besides the scope's own where
	// its threads may be linked (design, section 5.2).
	ShowIn []string
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
	// Empty makes the directory of a thread started confined: empty, no
	// repository, and outside every workspace. One that a daemon which died
	// made and did not record is taken again while it is still empty.
	Empty(ctx context.Context, thread control.ThreadID) (dir string, err error)
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
	// from is the "<client>:<id>" the input came from, if relayed.
	Resume(ctx context.Context, sessionID int64, principal control.Principal, from, key string, in control.Input) error
	// Wake starts the agent of an asleep session again with its
	// conversation and gives it no input, so that the session becomes idle.
	Wake(ctx context.Context, sessionID int64, principal control.Principal) error
	// OnPipes reports whether a session's agent is known to run on pipes,
	// with no terminal a person could attach to.
	OnPipes(sessionID int64) bool
	// AttachSocket returns the socket of the session host whose terminal a
	// person attaches to, once the host listens; it waits for a session
	// that is being started, until ctx ends. A session on pipes is
	// ErrNoTerminal.
	AttachSocket(ctx context.Context, sessionID int64) (string, error)
	// Terminal streams a session's terminal output, as control.API.Terminal
	// says, until ctx ends. A session on pipes is ErrNotFound.
	Terminal(ctx context.Context, sessionID int64, run string, after uint64) (<-chan control.TerminalChunk, error)
	// Device returns the name of the terminal a session's agent runs in,
	// such as /dev/pts/3; "" when it runs on pipes or no process runs.
	Device(ctx context.Context, sessionID int64) string
	// Shut closes a session: it applies session.Close with reason, in step
	// with what the runner reads of the agent, so nothing the agent reports
	// after it is read, and then stops the agent's process, when one runs,
	// and waits for it to end. A refused Close is returned and stops
	// nothing.
	Shut(ctx context.Context, sessionID int64, reason string) error
}

// Remover removes workspaces behind the proof of design section 4.6.
type Remover interface {
	// Remove proves that nothing in the workspace at dir would be lost and
	// then renames it into trash, returning where it went. Any error means
	// the workspace was not removed; a refusal of the proof implements
	// RemovalRefusal.
	Remove(ctx context.Context, dir string) (trash string, err error)
}

// RemovalRefusal is a Remover's error that says why the proof did not
// hold: the reason, and where in the workspace it was found (repo, and
// what kind of repository that is), with a detail for a person.
type RemovalRefusal interface {
	error
	RemovalRefused() (reason, repo, kind, detail string)
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
	From      string // the "<client>:<id>" the input came from, if relayed
	Key       string
	Input     control.Input
	// Context is what the thread said before, for a session that replaces
	// one whose conversation the agent no longer has; empty otherwise.
	Context string
	// Confined: the thread was started confined (StartConfined). Its
	// directory is made by Workspaces.Empty, not cloned; the runner reads
	// the thread's confinement for every launch itself.
	Confined bool

	placed *Placement // a workspace a Make placed before the daemon died
}

// FirstInput is the input a session starts with, as sessions.first_input
// holds it. The core stores it with the state change that starts or wakes
// the session, and sets sessions.launch; the runner clears launch when it
// launches the agent, and keeps the input until a turn reports its id.
type FirstInput struct {
	Principal control.Principal `json:"principal"`
	From      string            `json:"from,omitempty"`
	Key       string            `json:"key"`
	Text      string            `json:"text"`
	Sent      bool              `json:"sent,omitempty"`     // the turn was started for it, and it was handed to the codec
	Written   bool              `json:"written,omitempty"`  // it was written to the agent; see agent.Input.Written
	Wake      bool              `json:"wake,omitempty"`     // the session was asleep: its agent resumes
	NoInput   bool              `json:"no_input,omitempty"` // woken with no input, as for an attach: the agent is only started
	Dir       string            `json:"dir,omitempty"`      // the workspace, once it is made
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
	// SignIn makes dashboard sign-in links; nil when the instance has no
	// dashboard.
	SignIn SignIn
	// HostTerms lists and reads the terminals hataori does not own. Nil
	// when the config leaves them off: HostTerminals and HostScreen are
	// then refused for every caller.
	HostTerms HostTerms
	// Remover removes the workspaces of closed threads. It is the only way
	// the core removes a workspace; nil refuses every Remove.
	Remover Remover

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

// HostTerms lists and reads the terminals on the host, leaving out those
// a process under one of owned uses. It changes nothing. Screen's ok is
// false for a pane that List would not return.
type HostTerms interface {
	List(ctx context.Context, owned []int) ([]control.HostTerminal, error)
	Screen(ctx context.Context, pane string, owned []int) (screen string, ok bool, err error)
}

// SignIn makes single-use dashboard sign-in links. The session a link
// opens belongs to person, on the listener way names (empty: the loopback
// listener when there is one, else the web listener).
type SignIn interface {
	NewLink(ctx context.Context, person string, way control.Way) (string, error)
}

// MaySignIn reports whether person may get a dashboard sign-in link, and
// so whether a dashboard session of theirs is honoured. It is the one rule
// of who may sign in to the dashboard, for links asked for on the control
// socket and in chat alike: today the owner only (design, section 10).
func (c *Core) MaySignIn(person string) bool {
	return person != "" && person == c.People.Owner()
}

// ErrNoDashboard is returned by SignInLink when the instance has no
// dashboard.
var ErrNoDashboard = errors.New("the instance has no dashboard: add a \"dashboard\" section to its config")

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
	return a.refused(name, scope, "the person may not act in the scope")
}

// refused logs one line for a call refused because of the caller's person,
// with the client and the id it came from, and returns ErrForbidden. A
// caller without a person is a guest, whatever reason says.
func (a *api) refused(name, scope, reason string) error {
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
	if scope == "" {
		return fmt.Errorf("core: %s: %w", name, ErrForbidden)
	}
	return fmt.Errorf("core: %s in scope %q: %w", name, scope, ErrForbidden)
}

// marked returns in with the caller's mark, if it has one, as its first
// line.
func (a *api) marked(in control.Input) control.Input {
	if a.caller.Mark != "" {
		in.Text = a.caller.Mark + "\n" + in.Text
	}
	return in
}

// filterScope checks a read with filter f and returns the one scope f
// names, or "" for all of the caller's. A filter whose thread is in
// another scope than the one it names matches nothing, and is refused. A
// thread or link that does not exist names no scope: the read finds
// nothing.
func (a *api) filterScope(ctx context.Context, name string, f control.Filter) (string, error) {
	scope := f.Scope
	if f.Thread != 0 {
		ts, err := a.c.scopeOf(ctx, a.c.Store.DB, f.Thread)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		if f.Scope != "" && ts != "" && f.Scope != ts {
			a.c.Log.Warn("control call refused", "op", name, "principal", string(a.caller.Principal), "scope", f.Scope, "thread_scope", ts)
			return "", fmt.Errorf("core: %s: filter names scopes %q and %q: %w", name, f.Scope, ts, ErrForbidden)
		}
		if scope == "" {
			scope = ts
		}
	}
	return scope, a.check(control.OpRead, name, scope)
}

// scopeOf returns the scope of thread id; ErrNotFound when there is no
// such thread.
func (c *Core) scopeOf(ctx context.Context, q querier, id control.ThreadID) (string, error) {
	var scope string
	err := q.QueryRowContext(ctx, `SELECT scope FROM threads WHERE id = ?`, int64(id)).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("core: thread %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("core: thread %d: %w", id, err)
	}
	return scope, nil
}

// checkThread checks a call on thread id with check, in the thread's scope,
// and returns that scope.
func (a *api) checkThread(ctx context.Context, op control.Ops, name string, id control.ThreadID) (string, error) {
	scope, err := a.c.scopeOf(ctx, a.c.Store.DB, id)
	if err != nil {
		return "", err
	}
	return scope, a.check(op, name, scope)
}

// actThread is checkThread for a call that gives the agent input, or stops
// it: act in the thread's scope.
func (a *api) actThread(ctx context.Context, op control.Ops, name string, id control.ThreadID) (string, error) {
	scope, err := a.c.scopeOf(ctx, a.c.Store.DB, id)
	if err != nil {
		return "", err
	}
	return scope, a.act(op, name, scope)
}

func (a *api) Threads(ctx context.Context, f control.Filter) ([]control.ThreadInfo, error) {
	scope, err := a.filterScope(ctx, "Threads", f)
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
	if f.Thread != 0 {
		q += ` AND id = ?`
		args = append(args, int64(f.Thread))
	}
	if l := f.Link; l != nil {
		q += ` AND id IN (SELECT thread_id FROM links WHERE client = ? AND channel = ? AND thread = ?)`
		args = append(args, l.Client, l.Channel, l.Thread)
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
	sids := make([]int64, len(out))
	for i := range out {
		if sids[i], err = a.summary(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	a.c.terminals(ctx, out, sids)
	return out, nil
}

// terminals fills the terminal of each thread in ts that may have a process,
// asking the runner about the session in sids at the same index. The
// sessions' hosts are asked at once, within one bound, so that hosts that
// do not answer delay a list by that bound at most.
func (c *Core) terminals(ctx context.Context, ts []control.ThreadInfo, sids []int64) {
	ctx, cancel := context.WithTimeout(ctx, terminalsTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := range ts {
		if sids[i] == 0 || !hasProcess(ts[i].State) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts[i].Terminal = c.Runner.Device(ctx, sids[i])
		}()
	}
	wg.Wait()
}

// terminalsTimeout bounds how long terminals waits for the sessions' hosts.
const terminalsTimeout = time.Second

// summary fills the fields of t that come from its sessions, turns and
// events: the current session's agent, model and effort, the number of
// turns, the background of the last turn_ended event, and the last line of
// output unless the caller may not read text. It returns the current
// session, 0 for none; terminals fills the terminal.
func (a *api) summary(ctx context.Context, t *control.ThreadInfo) (int64, error) {
	db := a.c.Store.DB
	var sid int64
	err := db.QueryRowContext(ctx, `SELECT s.id, s.agent, s.model, s.effort,
		(SELECT count(*) FROM turns u JOIN sessions v ON v.id = u.session_id WHERE v.thread_id = s.thread_id)
		FROM sessions s WHERE s.thread_id = ? ORDER BY s.id DESC LIMIT 1`, int64(t.ID)).Scan(&sid, &t.Agent, &t.Model, &t.Effort, &t.Turns)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("core: thread summary: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		sid = 0
	}
	var (
		turn, used, size sql.NullInt64
		today            int64
		day              string
	)
	err = db.QueryRowContext(ctx, `SELECT tokens_turn, tokens_today, tokens_day, context_used, context_size FROM threads WHERE id = ?`,
		int64(t.ID)).Scan(&turn, &today, &day, &used, &size)
	if err != nil {
		return 0, fmt.Errorf("core: thread summary: %w", err)
	}
	if turn.Valid {
		t.Tokens = &control.TokenCount{Turn: turn.Int64}
		if now := a.c.now(); day == session.DayOf(now, a.c.Machine.Zone) {
			t.Tokens.Today = today
		}
	}
	if used.Valid && size.Valid {
		t.Context = &control.Context{Used: used.Int64, Size: size.Int64}
	}
	var data []byte
	err = db.QueryRowContext(ctx, `SELECT data FROM events WHERE thread_id = ? AND kind = 'turn_ended'
		ORDER BY pos DESC LIMIT 1`, int64(t.ID)).Scan(&data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("core: thread summary: %w", err)
	}
	if err == nil {
		var e struct {
			Background int `json:"background"`
		}
		json.Unmarshal(data, &e)
		t.Background = e.Background
	}
	if t.Links, err = a.c.links(ctx, t.ID); err != nil {
		return 0, err
	}
	if a.caller.NoText {
		t.Title = "" // a person wrote it
	} else {
		if t.Last, err = a.c.lastLine(ctx, t.ID); err != nil {
			return 0, err
		}
	}
	return sid, nil
}

// links returns the thread's links, the first one first; never nil.
func (c *Core) links(ctx context.Context, id control.ThreadID) ([]control.LinkRef, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT client, channel, thread FROM links WHERE thread_id = ? ORDER BY id`, int64(id))
	if err != nil {
		return nil, fmt.Errorf("core: links of thread %d: %w", id, err)
	}
	defer rows.Close()
	out := []control.LinkRef{}
	for rows.Next() {
		var l control.LinkRef
		if err := rows.Scan(&l.Client, &l.Channel, &l.Thread); err != nil {
			return nil, fmt.Errorf("core: links of thread %d: %w", id, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// hasProcess reports whether a thread in state s may have an agent
// process running: not asleep, closed, removed or failed.
func hasProcess(s control.State) bool {
	switch s {
	case control.Creating, control.Idle, control.Running, control.Waiting:
		return true
	}
	return false
}

// now is the machine's clock.
func (c *Core) now() time.Time {
	if c.Machine.Now != nil {
		return c.Machine.Now()
	}
	return time.Now()
}

// lastLine returns the last line of the thread's text output that is not
// blank, trimmed and cut to control.LastMax bytes at a character boundary.
// Each text event is a whole message of the agent, so a line never spans
// two events.
func (c *Core) lastLine(ctx context.Context, id control.ThreadID) (string, error) {
	rows, err := c.Store.DB.QueryContext(ctx,
		`SELECT data FROM events WHERE thread_id = ? AND kind = 'text' ORDER BY pos DESC`, int64(id))
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

const threadSelect = `SELECT id, scope, title, repo, started_by, state, created_at, updated_at FROM threads`

func scanThread(row interface{ Scan(...any) error }) (control.ThreadInfo, error) {
	var (
		t          control.ThreadInfo
		by, state  string
		created, u int64
	)
	if err := row.Scan(&t.ID, &t.Scope, &t.Title, &t.Repo, &by, &state, &created, &u); err != nil {
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
	if _, err := a.checkThread(ctx, control.OpRead, "Thread", id); err != nil {
		return control.ThreadDetail{}, err
	}
	t, err := scanThread(a.c.Store.DB.QueryRowContext(ctx, threadSelect+` WHERE id = ?`, int64(id)))
	if err != nil {
		return control.ThreadDetail{}, err
	}
	sid, err := a.summary(ctx, &t)
	if err != nil {
		return control.ThreadDetail{}, err
	}
	ts := []control.ThreadInfo{t}
	a.c.terminals(ctx, ts, []int64{sid})
	d := control.ThreadDetail{ThreadInfo: ts[0]}
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

func (a *api) ThreadEvents(ctx context.Context, id control.ThreadID, before uint64, n int) ([]control.Event, error) {
	if _, err := a.checkThread(ctx, control.OpRead, "ThreadEvents", id); err != nil {
		return nil, err
	}
	evs, err := a.c.Store.LastBefore(ctx, id, before, min(n, control.MaxEvents), a.mayRead)
	for i := range evs {
		evs[i] = a.readable(evs[i])
	}
	return evs, err
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
			SELECT s.id FROM sessions s WHERE s.thread_id = ? ORDER BY s.id DESC LIMIT 1)
		ORDER BY r.id DESC LIMIT 1`, int64(id)).Scan(&r.ID, &approval, &r.Text, &choices)
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
		`SELECT data FROM events WHERE thread_id = ? AND kind = 'text' ORDER BY pos DESC`, int64(id))
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
	return a.start(ctx, r, false)
}

// ErrConfinedKey is returned by Start and StartConfined when the key
// started a thread before that is confined while the call's is not, or the
// other way round: the thread the key names is not the one asked for.
var ErrConfinedKey = errors.New("the key started a thread that differs in confinement")

// StartConfined is Start, as caller, for a thread whose sessions all run
// confined (design, section 7.1): with no shell, file tools only in its own
// directory and only the MCP servers the runner gives each launch. Its
// directory is made empty by Workspaces.Empty instead of a clone, so r.Repo
// must be empty. It is not an operation of the control API: only a client
// inside the daemon starts such a thread.
func (c *Core) StartConfined(ctx context.Context, caller control.Caller, r control.StartRequest) (control.ThreadID, error) {
	if r.Repo != "" {
		return 0, fmt.Errorf("core: start in %q: a confined thread has no repository", r.Scope)
	}
	return c.As(caller).(*api).start(ctx, r, true)
}

// start is Start, and StartConfined when confined is set.
func (a *api) start(ctx context.Context, r control.StartRequest, confined bool) (control.ThreadID, error) {
	if err := a.act(control.OpStart, "Start", r.Scope); err != nil {
		return 0, err
	}
	// The title is the first line of what the caller wrote, not its mark.
	name := title(r.Input.Text)
	r.Input = a.marked(r.Input)
	sc, ok := a.c.Scopes[r.Scope]
	if !ok || !sc.Enabled {
		a.c.Log.Warn("start refused", "scope", r.Scope, "reason", "scope is not enabled")
		return 0, fmt.Errorf("core: start in %q: %w", r.Scope, ErrDisabled)
	}
	if r.Effort != "" && !slices.Contains(control.Efforts, r.Effort) {
		return 0, fmt.Errorf("core: start in %q: effort %q: %w", r.Scope, r.Effort, control.ErrUnknownEffort)
	}
	// The key is per scope, so a caller cannot reach another scope's thread
	// by reusing its key. Starts with one key run one at a time, so the
	// owner is asked for one thread only.
	inboxKey := "start:" + r.Scope + "\x00" + r.Key
	defer a.c.locks.lock(inboxKey)()
	if id, ok, err := a.c.started(ctx, inboxKey); err != nil || ok {
		if err != nil {
			return id, err
		}
		return id, a.c.sameConfinement(ctx, id, confined)
	}

	// The first link: in the scope's own channel, of the scope's owner.
	link := control.LinkRef{Client: sc.Owner, Channel: r.Scope, Thread: r.Thread}
	if r.Thread == "" {
		owner, ok := a.c.Owners[sc.Owner]
		if !ok {
			return 0, fmt.Errorf("core: start in %q: no owner %q", r.Scope, sc.Owner)
		}
		l, err := owner.NewThread(ctx, r.Scope, r.Key, name)
		if err != nil {
			return 0, fmt.Errorf("core: start in %q: owner: %w", r.Scope, err)
		}
		if l.Client != sc.Owner || l.Channel != r.Scope || l.Thread == "" {
			return 0, fmt.Errorf("core: start in %q: owner %q returned a link elsewhere: %s %s %s", r.Scope, sc.Owner, l.Client, l.Channel, l.Thread)
		}
		link = l
	}
	model := cmp.Or(r.Model, sc.Model, a.c.Model)
	effort := cmp.Or(r.Effort, sc.Effort, a.c.Effort)

	var (
		id        control.ThreadID
		sessionID int64
	)
	errDup := errors.New("the key was accepted before")
	err := a.c.Store.Update(ctx, func(tx *store.Tx) error {
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (scope, key, title, repo, started_by, state, created_at, updated_at, confined) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Scope, link.Thread, name, r.Repo, string(a.caller.Principal), string(control.Creating), now, now, confined)
		if err != nil {
			return fmt.Errorf("core: create thread: %w", err)
		}
		thread, _ := res.LastInsertId()
		id = control.ThreadID(thread)
		ref, _ := json.Marshal(id)
		ok, err := tx.Accept(ctx, inboxKey, ref)
		if err != nil {
			return err
		}
		if !ok {
			return errDup // nothing of this call is kept
		}
		// The first link, with the thread. A conversation links to one
		// thread at most, so a second thread in it is refused here.
		if _, err := tx.ExecContext(ctx, `INSERT INTO links (thread_id, client, channel, thread, made_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			thread, link.Client, link.Channel, link.Thread, string(a.caller.Principal), now); err != nil {
			return fmt.Errorf("core: create thread: link %s %s %s: %w", link.Client, link.Channel, link.Thread, err)
		}
		fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, From: a.caller.From, Key: r.Key, Text: r.Input.Text})
		res, err = tx.ExecContext(ctx,
			`INSERT INTO sessions (thread_id, agent, state, model, effort, first_input, launch, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			thread, a.c.Agent, string(control.Creating), model, effort, string(fi), now, now)
		if err != nil {
			return fmt.Errorf("core: create session: %w", err)
		}
		sessionID, _ = res.LastInsertId()
		d := map[string]any{"thread": thread, "session": sessionID, "repo": r.Repo,
			"principal": string(a.caller.Principal), "model": model, "effort": effort}
		if a.caller.From != "" {
			d["from"] = a.caller.From
		}
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: id, Scope: r.Scope, Kind: "thread_created", Data: data})
		return err
	})
	if errors.Is(err, errDup) { // a concurrent call with the same key won
		id, _, err := a.c.started(ctx, inboxKey)
		if err != nil {
			return id, err
		}
		return id, a.c.sameConfinement(ctx, id, confined)
	}
	if err != nil {
		return 0, err
	}

	if err := a.c.launch(ctx, Launch{Session: sessionID, Thread: id, Agent: a.c.Agent,
		Model: model, Effort: effort, Principal: a.caller.Principal, From: a.caller.From, Key: r.Key, Input: r.Input,
		Confined: confined}, r.Repo); err != nil {
		return id, err
	}
	return id, nil
}

// ConfinedSession is a session of a thread started confined.
type ConfinedSession struct {
	Session int64
	Thread  control.ThreadID
	Scope   string
}

// LiveConfined returns the sessions of threads started confined whose agent
// may be running or about to run: creating, idle, running or waiting.
func (c *Core) LiveConfined(ctx context.Context) ([]ConfinedSession, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT s.id, t.id, t.scope FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE t.confined = 1 AND s.state IN (?, ?, ?, ?) ORDER BY s.id`,
		string(control.Creating), string(control.Idle), string(control.Running), string(control.Waiting))
	if err != nil {
		return nil, fmt.Errorf("core: confined sessions: %w", err)
	}
	defer rows.Close()
	var out []ConfinedSession
	for rows.Next() {
		var s ConfinedSession
		if err := rows.Scan(&s.Session, &s.Thread, &s.Scope); err != nil {
			return nil, fmt.Errorf("core: confined sessions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LastConfined returns the newest thread of scope that was started
// confined, and its state; 0 when there is none.
func (c *Core) LastConfined(ctx context.Context, scope string) (control.ThreadID, control.State, error) {
	var (
		id    int64
		state string
	)
	err := c.Store.DB.QueryRowContext(ctx, `SELECT id, state FROM threads WHERE scope = ? AND confined = 1 ORDER BY id DESC LIMIT 1`,
		scope).Scan(&id, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("core: confined threads of %q: %w", scope, err)
	}
	return control.ThreadID(id), control.State(state), nil
}

// Confined reports whether thread id was started confined, as a
// supervisor's thread is.
func (c *Core) Confined(ctx context.Context, id control.ThreadID) (bool, error) {
	var confined bool
	err := c.Store.DB.QueryRowContext(ctx, `SELECT confined FROM threads WHERE id = ?`, int64(id)).Scan(&confined)
	if err != nil {
		return false, fmt.Errorf("core: thread %d: %w", id, err)
	}
	return confined, nil
}

// sameConfinement refuses a key that started a thread before whose
// confinement is not the one asked for now: an ordinary Start never
// returns a confined thread, and StartConfined never an ordinary one.
func (c *Core) sameConfinement(ctx context.Context, id control.ThreadID, confined bool) error {
	var was bool
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT confined FROM threads WHERE id = ?`, int64(id)).Scan(&was); err != nil {
		return fmt.Errorf("core: thread %d: %w", id, err)
	}
	if was != confined {
		c.Log.Warn("start refused", "thread", int64(id), "reason", ErrConfinedKey.Error())
		return fmt.Errorf("core: start: thread %d: %w", id, ErrConfinedKey)
	}
	return nil
}

// launch makes the workspace of a new session, unless l.Dir names it, and
// starts its agent. A failure is recorded as the session's end.
func (c *Core) launch(ctx context.Context, l Launch, repo string) error {
	var err error
	record := func(f FirstInput) error {
		f.Principal, f.From, f.Key, f.Text, f.Context = l.Principal, l.From, l.Key, l.Input.Text, l.Context
		fi, _ := json.Marshal(f)
		return c.Store.Update(ctx, func(tx *store.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ? WHERE id = ?`, string(fi), l.Session); err != nil {
				return err
			}
			if f.Dir == "" {
				return nil
			}
			// Where the thread's workspace is, read from here on.
			_, err := tx.ExecContext(ctx, `UPDATE threads SET workspace = ? WHERE id = ?`, f.Dir, int64(l.Thread))
			return err
		})
	}
	if l.Dir == "" && l.Confined {
		// No clone: an empty directory of its own.
		if l.Dir, err = c.Workspaces.Empty(ctx, l.Thread); err == nil {
			err = record(FirstInput{Dir: l.Dir})
		}
	}
	if l.Dir == "" && l.placed != nil && err == nil {
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
func (c *Core) resume(ctx context.Context, sid int64, principal control.Principal, from, key string, in control.Input) error {
	if err := c.Runner.Resume(ctx, sid, principal, from, key, in); err != nil {
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
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT s.id, s.agent, s.model, s.effort, s.first_input, t.id, t.repo, t.confined
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
		if err := rows.Scan(&x.l.Session, &x.l.Agent, &x.l.Model, &x.l.Effort, &fi, &x.l.Thread, &x.repo, &x.l.Confined); err != nil {
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
		if x.f.Wake && x.f.NoInput {
			if err = c.Runner.Wake(ctx, x.l.Session, x.f.Principal); err != nil {
				if ferr := c.Machine.Apply(ctx, x.l.Session, session.Input{Kind: session.Exit, Reason: "resume: " + err.Error()}); ferr != nil {
					c.Log.Error("recording a failed resume", "session", x.l.Session, "err", ferr)
				}
			}
		} else if x.f.Wake {
			err = c.resume(ctx, x.l.Session, x.f.Principal, x.f.From, x.f.Key, in)
		} else {
			x.l.Dir, x.l.Principal, x.l.From, x.l.Key, x.l.Input, x.l.Context = x.f.Dir, x.f.Principal, x.f.From, x.f.Key, in, x.f.Context
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
// Limits returns what the session machine kept in agent_limits, by agent.
func (a *api) Limits(ctx context.Context) ([]control.AgentLimits, error) {
	if err := a.check(control.OpRead, "Limits", ""); err != nil {
		return nil, err
	}
	rows, err := a.c.Store.DB.QueryContext(ctx, `SELECT agent, at, every, windows FROM agent_limits ORDER BY agent`)
	if err != nil {
		return nil, fmt.Errorf("core: limits: %w", err)
	}
	defer rows.Close()
	out := []control.AgentLimits{}
	for rows.Next() {
		var (
			l         control.AgentLimits
			at, every int64
			ws        []byte
		)
		if err := rows.Scan(&l.Agent, &at, &every, &ws); err != nil {
			return nil, fmt.Errorf("core: limits: %w", err)
		}
		if err := json.Unmarshal(ws, &l.Windows); err != nil {
			return nil, fmt.Errorf("core: limits of %s: %w", l.Agent, err)
		}
		l.At, l.Every = time.UnixMilli(at), every/1000
		out = append(out, l)
	}
	return out, rows.Err()
}

func (a *api) Workspace(ctx context.Context, id control.ThreadID) (control.WorkspaceState, error) {
	if _, err := a.checkThread(ctx, control.OpRead, "Workspace", id); err != nil {
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

// terminalSessionCheck is how often a Terminal stream looks whether the
// thread has a new current session, as after a session was replaced.
const terminalSessionCheck = 500 * time.Millisecond

// Terminal streams the terminal of the thread's current session. When the
// thread gets a new session, the stream goes on with it, starting with a
// chunk marked Reset; it ends when the new session has no terminal.
func (a *api) Terminal(ctx context.Context, id control.ThreadID, run string, after uint64) (<-chan control.TerminalChunk, error) {
	if _, err := a.checkThread(ctx, control.OpRead, "Terminal", id); err != nil {
		return nil, err
	}
	sid, _, err := a.c.current(ctx, a.c.Store.DB, id)
	if err != nil {
		return nil, err
	}
	sctx, stop := context.WithCancel(ctx)
	in, err := a.c.Runner.Terminal(sctx, sid, run, after)
	if err != nil {
		stop()
		return nil, err
	}
	out := make(chan control.TerminalChunk)
	go func() {
		defer func() { stop(); close(out) }()
		tick := time.NewTicker(terminalSessionCheck)
		defer tick.Stop()
		for {
			select {
			case ch, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- ch:
				case <-ctx.Done():
					return
				}
			case <-tick.C:
				now, _, err := a.c.current(ctx, a.c.Store.DB, id)
				if err != nil || now == sid {
					continue
				}
				stop() // the old session's stream ends; its chunks are dropped
				sctx, stop = context.WithCancel(ctx)
				if in, err = a.c.Runner.Terminal(sctx, now, "", 0); err != nil {
					return
				}
				sid = now
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// workspaceDir is the directory of the thread's workspace, as stored with
// the thread. It is never computed from the thread's ids: a thread made
// before thread numbers keeps the directory it had.
func (c *Core) workspaceDir(ctx context.Context, id control.ThreadID) (string, error) {
	var dir string
	err := c.Store.DB.QueryRowContext(ctx, `SELECT workspace FROM threads WHERE id = ?`, int64(id)).Scan(&dir)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("core: thread %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("core: thread %d: %w", id, err)
	}
	if dir == "" {
		return "", fmt.Errorf("core: thread %d has no workspace yet", id)
	}
	return dir, nil
}

// WorkspaceDir is the directory of the thread's workspace, as stored with
// the thread; an error when it has none yet.
func (c *Core) WorkspaceDir(ctx context.Context, id control.ThreadID) (string, error) {
	return c.workspaceDir(ctx, id)
}

// started returns the thread a Start key created, if it was used before.
// A key accepted before thread numbers holds the thread's scope and key.
func (c *Core) started(ctx context.Context, inboxKey string) (control.ThreadID, bool, error) {
	var data []byte
	err := c.Store.DB.QueryRowContext(ctx, `SELECT data FROM inbox WHERE key = ?`, inboxKey).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("core: inbox: %w", err)
	}
	var id control.ThreadID
	if json.Unmarshal(data, &id) == nil {
		return id, true, nil
	}
	var old struct{ Scope, Key string }
	if err := json.Unmarshal(data, &old); err != nil {
		return 0, false, fmt.Errorf("core: inbox: %w", err)
	}
	err = c.Store.DB.QueryRowContext(ctx, `SELECT id FROM threads WHERE scope = ? AND key = ? ORDER BY id LIMIT 1`, old.Scope, old.Key).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("core: inbox: the thread of %q: %w", inboxKey, err)
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
		SELECT s.id, s.state FROM sessions s WHERE s.thread_id = ? ORDER BY s.id DESC LIMIT 1`, int64(id)).Scan(&sid, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("core: thread %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return 0, "", fmt.Errorf("core: thread %d: %w", id, err)
	}
	return sid, control.State(state), nil
}

func (a *api) Send(ctx context.Context, id control.ThreadID, key string, in control.Input, busy control.Busy) error {
	scope, err := a.actThread(ctx, control.OpSend, "Send", id)
	if err != nil {
		return err
	}
	asked := busy
	if a.caller.RefuseBusy {
		busy = control.Refuse
	}
	// An answer to an open question is not marked: the agent's question
	// tool takes it as the answer, line by line or as a choice's label.
	given := in
	in = a.marked(in)
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
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		var state control.State
		var err error
		sid, state, err = a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		if state == control.Closed || state == control.Removed {
			// Nothing is accepted: a message does not wake a closed thread.
			return fmt.Errorf("core: send to %v: %w", id, ErrClosed)
		}
		if key != "" {
			data, _ := json.Marshal(map[string]any{"thread": id, "session": sid, "text": in.Text})
			ok, err := tx.Accept(ctx, "send:"+scope+"\x00"+key, data)
			if err != nil || !ok {
				return err // a repeated key is ignored, whatever Busy says
			}
		}
		if state == control.Running {
			switch busy {
			case control.Refuse:
				if asked != control.Refuse {
					a.c.Log.Warn("control call refused", "op", "Send", "principal", string(a.caller.Principal), "scope", scope,
						"reason", "the caller may not deliver into or interrupt a turn")
				}
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
			fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, From: a.caller.From, Key: key, Text: in.Text, Wake: true})
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
				if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Answer, Principal: a.caller.Principal, From: a.caller.From,
					Key: key, Text: given.Text, Request: req}); err != nil {
					return err
				}
				pending, err = pend(ctx, tx, sid, req, control.Answer{Text: given.Text})
				return err
			}
		}
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Send, Principal: a.caller.Principal, From: a.caller.From, Key: key, Text: in.Text}); err != nil {
			return err
		}
		pending, err = pend(ctx, tx, sid, "", in)
		return err
	})
	if err != nil || !accepted {
		return err
	}
	if answer != "" {
		return a.c.Runner.Answer(ctx, sid, pending, answer, control.Answer{Text: given.Text})
	}
	if wake {
		if err := a.c.resume(ctx, sid, a.caller.Principal, a.caller.From, key, in); err != nil {
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
		scope    string
		sid      int64
		agentReq string
		state    string
	)
	err := a.c.Store.DB.QueryRowContext(ctx, `
		SELECT t.scope, r.session_id, r.request, r.state FROM requests r
		JOIN sessions s ON s.id = r.session_id JOIN threads t ON t.id = s.thread_id
		WHERE r.handle = ?`, string(req)).Scan(&scope, &sid, &agentReq, &state)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		a.c.Log.Warn("control call refused", "op", "Answer", "principal", string(a.caller.Principal), "request", string(req), "reason", err)
		return fmt.Errorf("core: answer %q: %w", req, err)
	}
	if err := a.act(control.OpAnswer, "Answer", scope); err != nil {
		return err
	}
	if state != session.RequestOpen {
		return fmt.Errorf("core: answer %q: the request is %s: %w", req, state, ErrNotOpen)
	}
	// The machine refuses it, atomically, if it was closed since.
	var pending int64
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Answer, Principal: a.caller.Principal, From: a.caller.From,
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
	if _, err := a.actThread(ctx, control.OpStop, "Stop", id); err != nil {
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
	if !slices.Contains(a.caller.Scopes, e.Scope) {
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
	if _, err := a.filterScope(ctx, "Events", f); err != nil {
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

func (a *api) SignInLink(ctx context.Context, way control.Way) (string, error) {
	if err := a.check(control.OpSignIn, "SignInLink", ""); err != nil {
		return "", err
	}
	person := string(a.caller.Principal)
	if !a.c.MaySignIn(person) {
		return "", a.refused("SignInLink", "", "the person may not sign in to the dashboard")
	}
	if a.c.SignIn == nil {
		return "", fmt.Errorf("core: SignInLink: %w", ErrNoDashboard)
	}
	switch way {
	case "", control.WayLocal, control.WayWeb:
	default:
		return "", fmt.Errorf("core: SignInLink: %q is not a listener: say %q or %q", way, control.WayLocal, control.WayWeb)
	}
	return a.c.SignIn.NewLink(ctx, person, way)
}

// checkHost allows HostTerminals and HostScreen: only when they are on, and
// only to the owner's caller with OpHost.
func (a *api) checkHost(name string) error {
	if a.c.HostTerms != nil && a.c.People != nil && a.c.People.Owner() != "" &&
		a.caller.Principal == control.Principal(a.c.People.Owner()) {
		return a.check(control.OpHost, name, "")
	}
	a.c.Log.Warn("control call refused", "op", name, "principal", string(a.caller.Principal))
	return fmt.Errorf("core: %s: %w", name, ErrForbidden)
}

// hostPIDs are the session hosts that may hold a terminal now.
func (c *Core) hostPIDs(ctx context.Context) ([]int, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT host_pid FROM sessions WHERE host_pid > 0 AND state NOT IN (?, ?, ?)`,
		string(control.Asleep), string(control.Closed), string(control.Removed))
	if err != nil {
		return nil, fmt.Errorf("core: %w", err)
	}
	defer rows.Close()
	var pids []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("core: %w", err)
		}
		pids = append(pids, pid)
	}
	return pids, rows.Err()
}

func (a *api) HostTerminals(ctx context.Context) ([]control.HostTerminal, error) {
	if err := a.checkHost("HostTerminals"); err != nil {
		return nil, err
	}
	owned, err := a.c.hostPIDs(ctx)
	if err != nil {
		return nil, err
	}
	return a.c.HostTerms.List(ctx, owned)
}

func (a *api) HostScreen(ctx context.Context, pane string) (string, error) {
	if err := a.checkHost("HostScreen"); err != nil {
		return "", err
	}
	owned, err := a.c.hostPIDs(ctx)
	if err != nil {
		return "", err
	}
	screen, ok, err := a.c.HostTerms.Screen(ctx, pane, owned)
	if err == nil && !ok {
		err = fmt.Errorf("core: HostScreen: tmux pane %q: %w", pane, ErrNotFound)
	}
	return screen, err
}

// attachTimeout bounds how long AttachTerminal waits for the session host
// of a thread that is being started or woken.
const attachTimeout = 90 * time.Second

// AttachTerminal readies a thread for a person's terminal on the host: the
// owner only, and only with OpAttach. A thread that is closed, removed or
// failed, and an agent known to have no terminal, are refused before
// anything changes. An asleep thread is woken with no input first.
func (a *api) AttachTerminal(ctx context.Context, id control.ThreadID) (control.Attachment, error) {
	const name = "AttachTerminal"
	scope, err := a.checkThread(ctx, control.OpAttach, name, id)
	if err != nil {
		return control.Attachment{}, err
	}
	if owner := a.c.People.Owner(); owner == "" || a.caller.Principal != control.Principal(owner) {
		return control.Attachment{}, a.refused(name, scope, "only the owner may attach a terminal")
	}
	var (
		sid    int64
		number = int64(id)
		wake   bool
	)
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		wake = false
		var (
			state control.State
			err   error
		)
		if sid, state, err = a.c.current(ctx, tx, id); err != nil {
			return err
		}
		switch state {
		case control.Closed, control.Removed:
			return fmt.Errorf("core: attach to thread %d: it is %s: %w: %w", number, state, ErrClosed, session.ErrRefused)
		case control.Failed:
			return fmt.Errorf("core: attach to thread %d: its session failed and has no agent running: %w", number, session.ErrRefused)
		}
		if a.c.Runner.OnPipes(sid) {
			return fmt.Errorf("core: attach to thread %d: %w: %w", number, ErrNoTerminal, session.ErrRefused)
		}
		if state != control.Asleep {
			// The idle period starts again now, in the transaction the idle
			// check also writes in: the session is not put to sleep between
			// this answer and the terminal reaching its host, which then
			// reports it attached.
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = max(updated_at, ?) WHERE id = ?`, a.c.now().UnixMilli(), sid)
			return err
		}
		wake = true
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Wake}); err != nil {
			return err
		}
		fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, Wake: true, NoInput: true})
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ?, launch = 1 WHERE id = ?`, string(fi), sid); err != nil {
			return fmt.Errorf("core: wake: %w", err)
		}
		return nil
	})
	if err != nil {
		return control.Attachment{}, err
	}
	if wake {
		if err := a.c.Runner.Wake(ctx, sid, a.caller.Principal); err != nil {
			if ferr := a.c.Machine.Apply(ctx, sid, session.Input{Kind: session.Exit, Reason: "resume: " + err.Error()}); ferr != nil {
				a.c.Log.Error("recording a failed resume", "session", sid, "err", ferr)
			}
			return control.Attachment{}, fmt.Errorf("core: attach to thread %d: resume: %w", number, err)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	sock, err := a.c.Runner.AttachSocket(wctx, sid)
	if err != nil {
		return control.Attachment{}, fmt.Errorf("core: attach to thread %d: %w", number, err)
	}
	a.c.Log.Info("a terminal attaches", "scope", scope, "thread", number, "person", string(a.caller.Principal))
	return control.Attachment{Socket: sock, Number: number}, nil
}
