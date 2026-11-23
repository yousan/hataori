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
	"strconv"
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
	Agent   string // the registered name of the agent of new threads; empty for Core.Agent
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
	// Confined returns the directory of a thread started confined, dir, with
	// no repository and outside every workspace: made empty when it is not
	// there, and used as it is when it is, never emptied, moved or written
	// to. An empty dir is the thread's own, as before a scope had one.
	Confined(ctx context.Context, thread control.ThreadID, dir string) (string, error)
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
	// Interrupt stops a session's running turn. A turn other than 0 names
	// it: nothing is interrupted unless it is the session's running turn,
	// checked in the step that sends the interrupt, and the error then
	// wraps session.ErrRefused. The check is against the turns the runner
	// has recorded, read from the agent every poll: a turn the agent began
	// by itself, or that a person typed at its terminal, before the runner
	// read the named one's end can still get the interrupt of an agent
	// whose interrupt names no turn (#494). by, when it has a Principal, is
	// whose Stop it is: in the same step, the person is recorded on the
	// turn interrupted, unless that turn names someone already, and a stop
	// event is written with by and the turn; the zero Stopper is an
	// interrupt nobody asked for as a stop, which records nothing.
	Interrupt(ctx context.Context, sessionID, turn int64, by session.Stopper) error
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

// Restorer brings back the directory of a removed or closed thread for a
// reopen (design, section 5.1). Neither of its calls replaces anything: a
// directory at dir, in any form, is refused and left as it is.
type Restorer interface {
	// Restore takes the workspace that a Remover's Remove moved to trash
	// back to dir. An entry no longer there, as after the purge, is an
	// error that wraps fs.ErrNotExist, and nothing changes. Right before
	// the rename, placed, when not nil, is called with what will identify
	// the directory at dir, as Workspaces.Make's is; when it fails, the
	// entry stays in trash.
	Restore(ctx context.Context, trash, dir string, placed func(Placement) error) error
	// Trashed finds the entry in trash of the workspace of thread that was
	// at dir, by its name, for a removal that recorded none; "" when there
	// is none. Several, or a place whose entry's name does not tell whose
	// it was, is an error or "": it never guesses.
	Trashed(thread control.ThreadID, dir string) (string, error)
	// Remake makes the workspace of thread at dir again: a new clone of
	// repo. placed is called as Restore's is.
	Remake(ctx context.Context, thread control.ThreadID, repo, dir string, placed func(Placement) error) error
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
	// NoInput: the agent is only started, with no first input, as for a
	// session that replaces one woken with none.
	NoInput bool
	// Confined: the thread was started confined (StartConfined). Its
	// directory, Dir, is given by Workspaces.Confined, not cloned; the
	// runner reads the thread's confinement for every launch itself.
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
	Dir       string            `json:"dir,omitempty"`      // the workspace, once it is made; a confined thread's from its start
	// Link is the link the input came through, if any (design, section 5.2).
	Link *control.LinkRef `json:"link,omitempty"`
	// Via is the way of the daemon's own the input came through
	// (control.Caller.Via), if any.
	Via string `json:"via,omitempty"`
	// Context goes before Text when the input is handed to the agent; the
	// input event records Text alone. See Launch.Context.
	Context string `json:"context,omitempty"`
	// Before and After are the text the plugins added around Text
	// (control.Input.Before): the agent gets them with it, and the input
	// event keeps them beside it.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
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

// AgentDefaults are the model and effort of the threads of one agent, from
// its adapter's section of the config.
type AgentDefaults struct{ Model, Effort string }

// Core is the core of one instance. It has no operations of its own: use
// As to get an API bound to a caller.
type Core struct {
	Store   *store.Store
	Machine *session.Machine
	// ApprovedStart, when set, checks the Start that a person approved as
	// a proposal (control.Caller.Propose) before it is made, as the
	// proposer's connection checked it when it was proposed: the daemon
	// gives the supervisor's repositories (#646). An error refuses it, and
	// the proposal's outcome is failed. Nil checks nothing.
	ApprovedStart func(scope, repo string) error
	Scopes        map[string]Scope              // by scope key
	Owners        map[string]control.ScopeOwner // by client name
	// People resolves a caller's From to a person and says who may act in
	// a scope. Without it no input is allowed.
	People     *people.Directory
	Workspaces Workspaces
	Runner     Runner
	// Agents are the configured agents by registered name, with the defaults
	// of each. Nil: Agent is the only one, with no defaults of its own.
	Agents map[string]AgentDefaults
	Agent  string // the registered name of the agent new sessions use
	Model  string // default model of a scope without one
	Effort string // default effort of a scope without one
	Log    *slog.Logger
	// SignIn makes dashboard sign-in links; nil when the instance has no
	// dashboard.
	SignIn SignIn
	// HostTerms lists and reads the terminals hataori does not own. Nil
	// when the config leaves them off: HostTerminals and HostScreen are
	// then refused for every caller.
	HostTerms HostTerms
	// Windows has the last report of each window a person started, from
	// the hooks of the plugin they installed; nil when nothing takes the
	// reports. TakeIn is then refused: there is no window to take in.
	Windows Windows
	// HostInput types into tmux panes on the host that hataori does not
	// own. Nil when the config's host_input is off: OperateHost is then
	// refused for every caller, as it is without HostTerms.
	HostInput HostInput
	// HostInputIdle ends an operation of a pane after this long without a
	// key; DefaultHostInputIdle when 0.
	HostInputIdle time.Duration
	// Stopping is closed when the daemon stops: every operation of a pane
	// ends with control.HostStopping, and OperateHost is refused. Nil
	// never closes.
	Stopping <-chan struct{}
	// Remover removes the workspaces of closed threads. It is the only way
	// the core removes a workspace; nil refuses every Remove.
	Remover Remover
	// Restorer takes a removed thread's workspace back from trash, or makes
	// it again, for a reopen; nil refuses a reopen that needs either.
	Restorer Restorer
	// Hooks runs the plugins' handlers on the input hook points (design,
	// section 7.3) inside Start and Send, before the input is accepted.
	// Nil: there are none, and input is not held up.
	Hooks control.Hooks

	locks   keyedMutex
	hostOps hostOpSet
	layouts layouts
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
	// View is Screen with the cursor's position.
	View(ctx context.Context, pane string, owned []int) (v control.HostView, ok bool, err error)
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
// dashboard (control.ErrNoDashboard).
var ErrNoDashboard = control.ErrNoDashboard

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
// and the operation name when the caller is limited (granted), and scope,
// when it is not empty, must be one of the caller's. A refusal writes one
// log line.
func (a *api) check(op control.Ops, name, scope string) error {
	if err := a.granted(name, scope); err != nil {
		return err
	}
	if a.caller.May.Has(op) && (scope == "" || slices.Contains(a.caller.Scopes, scope)) {
		if op == control.OpRead && scope != "" && !a.personSees(scope) {
			return a.refused(name, scope, "the person may not act in the scope")
		}
		return nil
	}
	a.c.Log.Warn("control call refused", a.via("op", name, "principal", string(a.caller.Principal), "scope", scope)...)
	return fmt.Errorf("core: %s in scope %q: %w", name, scope, ErrForbidden)
}

// via adds the caller's Via, when it has one, to the attributes of a log
// line about one of its calls.
func (a *api) via(attrs ...any) []any {
	if a.caller.Via != "" {
		attrs = append(attrs, "via", a.caller.Via)
	}
	return attrs
}

// protocolNames are the names in docs/protocol.md of the operations whose
// method has another name.
var protocolNames = map[string]string{"CloseThread": "Close", "RemoveThread": "Remove", "ReopenThread": "Reopen"}

// granted refuses an operation that the caller's Limit does not name, by
// its name in the protocol, with one log line naming the operation, the
// scope and the caller. A caller without a Limit is granted every
// operation; check then decides as before.
func (a *api) granted(name, scope string) error {
	l := a.caller.Limit
	if l == nil {
		return nil
	}
	if p, ok := protocolNames[name]; ok {
		name = p
	}
	if slices.Contains(l.Operations, name) {
		return nil
	}
	attrs := []any{"op", name, "scope", scope, "principal", string(a.caller.Principal), "reason", "the operation is not granted"}
	if a.caller.From != "" {
		attrs = append(attrs, "from", a.caller.From)
	}
	a.c.Log.Warn("control call refused", a.via(attrs...)...)
	// The answer names the scope only when it is one of the caller's: a
	// thread's or a request's scope the caller may not see is in the log
	// line alone (#479).
	if scope == "" || !slices.Contains(a.caller.Scopes, scope) {
		return fmt.Errorf("core: %s: not granted: %w", name, ErrForbidden)
	}
	return fmt.Errorf("core: %s in scope %q: not granted: %w", name, scope, ErrForbidden)
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

// sees reports whether the caller may read scope: it is one of the
// caller's scopes, and personSees.
func (a *api) sees(scope string) bool {
	return slices.Contains(a.caller.Scopes, scope) && a.personSees(scope)
}

// personSees reports whether the caller may read scope as far as its
// person goes: always, unless the caller has ReadAsPerson, whose reads
// need the person to be allowed in the scope as its input does.
func (a *api) personSees(scope string) bool {
	return !a.caller.ReadAsPerson || a.c.People.May(string(a.caller.Principal), scope)
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
	a.c.Log.Warn("control call refused", a.via(attrs...)...)
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

// Input is the first input as Start or Send gave it: its text, link, the
// text the plugins added, and the caller's Via.
func (f FirstInput) Input() control.Input {
	return control.Input{Text: f.Text, Link: f.Link, Before: f.Before, After: f.After, Via: f.Via}
}

// The input hook points, in the order they run (design, section 7.3).
const (
	PointBeforeAgentRun    = "before_agent_run"
	PointBeforePromptBuild = "before_prompt_build"
)

// inputHooks runs the input hook points for in, an input of op (Start or
// Send) to thread, 0 for a Start, in scope, before the core accepts it:
// before_agent_run, which may stop it, then before_prompt_build, whose
// added text it returns in in.Before and in.After, never the caller's. A
// stopped input is refused with a *control.Stopped and one log line, and
// nothing of it is kept. Without Hooks it calls nothing.
func (a *api) inputHooks(ctx context.Context, op string, thread control.ThreadID, scope string, in control.Input) (control.Input, error) {
	return a.runInputHooks(ctx, op, thread, scope, in, PointBeforeAgentRun, PointBeforePromptBuild)
}

// runInputHooks is inputHooks at points.
func (a *api) runInputHooks(ctx context.Context, op string, thread control.ThreadID, scope string, in control.Input, points ...string) (control.Input, error) {
	in.Before, in.After = "", ""
	if a.c.Hooks == nil {
		return in, nil
	}
	for _, point := range points {
		r := a.c.Hooks.Run(ctx, control.HookCall{Point: point, Thread: thread, Scope: scope, Text: in.Text})
		if err := ctx.Err(); err != nil {
			return in, err // the caller gave up: no plugin stopped it
		}
		if r.Stopped {
			attrs := []any{"op", op, "scope", scope, "point", point, "plugin", r.By, "failed", r.Failed,
				"principal", string(a.caller.Principal)}
			if thread != 0 {
				attrs = append(attrs, "thread", int64(thread))
			}
			if a.caller.From != "" {
				attrs = append(attrs, "from", a.caller.From)
			}
			a.c.Log.Warn("input stopped by a plugin", attrs...) // without the text, and the reason, which may quote it
			return in, fmt.Errorf("core: %s in %q: %w", op, scope, &control.Stopped{Point: point, Plugin: r.By, Reason: r.Reason, Failed: r.Failed})
		}
		if point == PointBeforePromptBuild {
			in.Before, in.After = r.Before, r.After
		}
	}
	return in, nil
}

// noThread is a thread number no thread has.
const noThread control.ThreadID = -1

// filterScope checks a read with filter f and returns the one scope f
// names, or "" for all of the caller's. A thread or link that does not
// exist names no scope: the read finds nothing. A thread, or a link's
// thread, of a scope the caller may not see is read as a thread that does
// not exist: f is changed to name noThread, so that the answer, the
// caught_up mark's position too, tells the caller neither that the thread
// exists nor its scope. The log line names both scopes. A filter whose
// thread is in another of the caller's scopes than the one it names
// matches nothing, and is refused.
func (a *api) filterScope(ctx context.Context, name string, f *control.Filter) (string, error) {
	if err := a.granted(name, f.Scope); err != nil {
		return "", err
	}
	scope := f.Scope
	if f.Thread != 0 {
		ts, err := a.c.scopeOf(ctx, a.c.Store.DB, f.Thread)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		if ts != "" && !a.sees(ts) {
			a.unseen(name, *f, f.Thread, ts)
			f.Thread, ts = noThread, ""
		}
		if f.Scope != "" && ts != "" && f.Scope != ts {
			a.c.Log.Warn("control call refused", "op", name, "principal", string(a.caller.Principal), "scope", f.Scope, "thread_scope", ts)
			return "", fmt.Errorf("core: %s: filter names scopes %q and %q: %w", name, f.Scope, ts, ErrForbidden)
		}
		if scope == "" {
			scope = ts
		}
	}
	if l := f.Link; l != nil && f.Thread != noThread {
		var id control.ThreadID
		var ts string
		err := a.c.Store.DB.QueryRowContext(ctx, `SELECT t.id, t.scope FROM links l JOIN threads t ON t.id = l.thread_id
			WHERE l.client = ? AND l.channel = ? AND l.thread = ?`, l.Client, l.Channel, l.Thread).Scan(&id, &ts)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("core: %s: link: %w", name, err)
		}
		if err == nil && !a.sees(ts) {
			a.unseen(name, *f, id, ts)
			f.Thread = noThread
		}
	}
	return scope, a.check(control.OpRead, name, scope)
}

// unseen logs a read whose filter f names thread id of scope ts, which the
// caller may not see: with the filter's scope, the thread's, and who asked.
func (a *api) unseen(name string, f control.Filter, id control.ThreadID, ts string) {
	attrs := []any{"op", name, "principal", string(a.caller.Principal), "scope", f.Scope, "thread", int64(id), "thread_scope", ts}
	if client, cid, ok := people.Split(a.caller.From); ok {
		attrs = append(attrs, "client", client, "id", cid)
	}
	a.c.Log.Warn("control read names a thread the caller may not see", attrs...)
}

// scopeOf returns the scope of thread id; ErrNotFound when there is no
// such thread.
func (c *Core) scopeOf(ctx context.Context, q querier, id control.ThreadID) (string, error) {
	var scope string
	err := q.QueryRowContext(ctx, `SELECT scope FROM threads WHERE id = ?`, int64(id)).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return "", noThreadErr(id)
	}
	if err != nil {
		return "", fmt.Errorf("core: thread %d: %w", id, err)
	}
	return scope, nil
}

// checkThread checks a call on thread id with check, in the thread's scope,
// and returns that scope.
func (a *api) checkThread(ctx context.Context, op control.Ops, name string, id control.ThreadID) (string, error) {
	scope, err := a.threadScope(ctx, name, id)
	if err != nil {
		return "", err
	}
	return scope, a.check(op, name, scope)
}

// threadScope is the scope of thread id for operation name. A caller not
// granted the operation is refused first, whether the thread exists or
// not, so that it learns nothing of the thread; the refusal is logged
// with the thread's scope when there is one. A thread of a scope that is
// not one of the caller's is answered as a thread that does not exist,
// ErrNotFound with the same message, so that the caller learns neither
// that it exists nor its scope (#479); the log line names the scope.
func (a *api) threadScope(ctx context.Context, name string, id control.ThreadID) (string, error) {
	scope, err := a.c.scopeOf(ctx, a.c.Store.DB, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if gerr := a.granted(name, scope); gerr != nil {
		return "", gerr
	}
	if err == nil && !slices.Contains(a.caller.Scopes, scope) {
		a.hidden(name, "thread", strconv.FormatInt(int64(id), 10), scope)
		return "", noThreadErr(id)
	}
	return scope, err
}

// noThreadErr is the error for thread id when there is no such thread, as
// scopeOf returns it.
func noThreadErr(id control.ThreadID) error {
	return fmt.Errorf("core: thread %d: %w", id, ErrNotFound)
}

// hidden logs a call that names something (a thread, a request, a link) of
// scope, which is not one of the caller's: the call is answered as if it
// did not exist.
func (a *api) hidden(name, what, which, scope string) {
	attrs := []any{"op", name, "principal", string(a.caller.Principal), what, which, "thread_scope", scope,
		"reason", "a scope the caller may not see"}
	if client, cid, ok := people.Split(a.caller.From); ok {
		attrs = append(attrs, "client", client, "id", cid)
	}
	a.c.Log.Warn("control call refused", a.via(attrs...)...)
}

// actThread is checkThread for a call that gives the agent input, or stops
// it: act in the thread's scope.
func (a *api) actThread(ctx context.Context, op control.Ops, name string, id control.ThreadID) (string, error) {
	scope, err := a.threadScope(ctx, name, id)
	if err != nil {
		return "", err
	}
	return scope, a.act(op, name, scope)
}

func (a *api) Threads(ctx context.Context, f control.Filter) ([]control.ThreadInfo, error) {
	scope, err := a.filterScope(ctx, "Threads", &f)
	if err != nil {
		return nil, err
	}
	var scopes []string
	for _, s := range a.caller.Scopes {
		if a.personSees(s) {
			scopes = append(scopes, s)
		}
	}
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
		if err := a.c.starter(ctx, &out[i]); err != nil {
			return nil, err
		}
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
		if t.Name, err = a.c.linkName(ctx, t.Links); err != nil {
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

// starter names who started t as the person, in place of what its row
// records. A name of today's people is kept. Otherwise the id the start
// came from, which its thread_created event records since the daemon kept
// it (#222), names the person who lists it; else the recorded value does
// (people.Directory.Recorded); else it stays as it is.
func (c *Core) starter(ctx context.Context, t *control.ThreadInfo) error {
	by := string(t.StartedBy)
	if by == "" || c.People.Listed(by) {
		return nil
	}
	var data []byte
	err := c.Store.DB.QueryRowContext(ctx, `SELECT data FROM events WHERE thread_id = ? AND kind = 'thread_created' ORDER BY pos LIMIT 1`, int64(t.ID)).Scan(&data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("core: thread %d: its start: %w", t.ID, err)
	}
	var created struct {
		From string `json:"from"`
	}
	if len(data) > 0 && json.Unmarshal(data, &created) == nil && created.From != "" {
		if name, ok := c.People.Resolve(created.From); ok {
			t.StartedBy = control.Principal(name)
			return nil
		}
	}
	if name, ok := c.People.Recorded(by, c.Scopes[t.Scope].Owner); ok {
		t.StartedBy = control.Principal(name)
	}
	return nil
}

func (a *api) Thread(ctx context.Context, id control.ThreadID, recent, events int) (control.ThreadDetail, error) {
	if _, err := a.checkThread(ctx, control.OpRead, "Thread", id); err != nil {
		return control.ThreadDetail{}, err
	}
	t, err := scanThread(a.c.Store.DB.QueryRowContext(ctx, threadSelect+` WHERE id = ?`, int64(id)))
	if err != nil {
		return control.ThreadDetail{}, err
	}
	if err := a.c.starter(ctx, &t); err != nil {
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
			d.Requests = append(d.Requests, r)
		}
	}
	rows, err := a.c.Store.DB.QueryContext(ctx, `SELECT handle, target, reason FROM approvals WHERE asks_in = ? AND state = 'open' ORDER BY id`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var r control.Request
		var target int64
		var reason string
		if err := rows.Scan(&r.ID, &target, &reason); err != nil {
			rows.Close()
			return d, err
		}
		r.Kind = control.Approval
		if !a.caller.NoText {
			r.Text = fmt.Sprintf("Interrupt thread %d? %s", target, reason)
		}
		r.Choices = []control.Choice{{Label: "Approve", Token: "approve"}, {Label: "Refuse", Token: "refuse"}}
		d.Requests = append(d.Requests, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return d, err
	}
	rows.Close()
	if d.Requests, err = a.c.openProposals(ctx, id, d.Requests, a.caller.NoText); err != nil {
		return d, err
	}
	if !a.caller.NoText {
		if d.Recent, err = a.c.recentText(ctx, id, recent); err != nil {
			return d, err
		}
	}
	if d.Cancelled, err = a.cancelled(ctx, id); err != nil {
		return d, err
	}
	d.Events, err = a.c.Store.Last(ctx, id, min(events, control.MaxEvents), a.mayRead)
	for i := range d.Events {
		d.Events[i] = a.readable(d.Events[i])
	}
	return d, err
}

// cancelled is thread id's last posts that a plugin cancelled, from the
// post_cancelled events the chat front end wrote (#352), newest last; a
// plugin's reason, which is text, is left out for a caller that may not
// read text.
func (a *api) cancelled(ctx context.Context, id control.ThreadID) ([]control.CancelledPost, error) {
	rows, err := a.c.Store.DB.QueryContext(ctx, `SELECT time, data FROM events WHERE thread_id = ? AND kind = 'post_cancelled'
		ORDER BY pos DESC LIMIT ?`, int64(id), control.CancelledMax)
	if err != nil {
		return nil, fmt.Errorf("core: cancelled posts: %w", err)
	}
	defer rows.Close()
	var out []control.CancelledPost
	for rows.Next() {
		var ms int64
		var data []byte
		if err := rows.Scan(&ms, &data); err != nil {
			return nil, fmt.Errorf("core: cancelled posts: %w", err)
		}
		var c control.CancelledPost
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		c.Time = time.UnixMilli(ms).UTC()
		if a.caller.NoText {
			c.Reason = "" // as the event's text field is left out
		}
		out = append(out, c)
	}
	slices.Reverse(out)
	return out, rows.Err()
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

// requestScope is the scope of the thread of request req, a question or an
// approval; "" when there is no such request.
func (c *Core) requestScope(ctx context.Context, req control.RequestID) string {
	var scope string
	if c.Store.DB.QueryRowContext(ctx, `SELECT t.scope FROM requests r JOIN sessions s ON s.id = r.session_id
		JOIN threads t ON t.id = s.thread_id WHERE r.handle = ?`, string(req)).Scan(&scope) == nil {
		return scope
	}
	if c.Store.DB.QueryRowContext(ctx, `SELECT t.scope FROM approvals a JOIN threads t ON t.id = a.asks_in WHERE a.handle = ?`, string(req)).Scan(&scope) == nil {
		return scope
	}
	c.Store.DB.QueryRowContext(ctx, `SELECT t.scope FROM proposals p JOIN threads t ON t.id = p.asks_in WHERE p.handle = ?`, string(req)).Scan(&scope)
	return scope
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

// recentSep goes between two text events in recentText: each is a whole
// agent message, and joined without it the end of one message ran into the
// next (#420). It is part of the protocol (ThreadDetail.recent).
const recentSep = "\n\n"

// recentText returns the end of the thread's text output: its text events,
// oldest first, with recentSep between them, cut to its last max bytes at
// the start of a character.
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
		n += len(e.Text) + len(recentSep)
	}
	slices.Reverse(parts)
	s := strings.Join(parts, recentSep)
	if len(s) > max {
		i := len(s) - max
		for i < len(s) && !utf8.RuneStart(s[i]) {
			i++
		}
		s = s[i:]
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
	return a.start(ctx, r, "")
}

// ErrConfinedKey is returned by Start and StartConfined when the key
// started a thread before that is confined while the call's is not, or the
// other way round: the thread the key names is not the one asked for.
var ErrConfinedKey = errors.New("the key started a thread that differs in confinement")

// StartConfined is Start, as caller, for a thread whose sessions all run
// confined (design, section 7.1): with no shell, file tools only in its own
// directory and only the MCP servers the runner gives each launch. Its
// directory is dir, given by Workspaces.Confined instead of a clone, so
// r.Repo must be empty: several threads may be started in one dir, one
// after another, as a scope's supervisor threads are (design, section 7.1,
// "Its directory and its notes"). It is not an operation of the control
// API: only a client inside the daemon starts such a thread.
func (c *Core) StartConfined(ctx context.Context, caller control.Caller, r control.StartRequest, dir string) (control.ThreadID, error) {
	if r.Repo != "" {
		return 0, fmt.Errorf("core: start in %q: a confined thread has no repository", r.Scope)
	}
	if dir == "" {
		return 0, fmt.Errorf("core: start in %q: a confined thread needs its directory", r.Scope)
	}
	return c.As(caller).(*api).start(ctx, r, dir)
}

// start is Start, and StartConfined in dir when dir is set.
func (a *api) start(ctx context.Context, r control.StartRequest, dir string) (control.ThreadID, error) {
	confined := dir != ""
	if err := a.act(control.OpStart, "Start", r.Scope); err != nil {
		return 0, err
	}
	if err := a.proposalKey("Start", r.Scope, r.Key); err != nil {
		return 0, err
	}
	// The title is the first line of what the caller wrote, not its mark.
	name := title(r.Input.Text)
	given := r.Input
	r.Input = a.marked(r.Input)
	sc, ok := a.c.Scopes[r.Scope]
	if !ok || !sc.Enabled {
		a.c.Log.Warn("start refused", "scope", r.Scope, "reason", "scope is not enabled")
		return 0, fmt.Errorf("core: start in %q: %w", r.Scope, ErrDisabled)
	}
	chosen := cmp.Or(r.Agent, sc.Agent, a.c.Agent)
	if confined {
		chosen = cmp.Or(r.Agent, a.c.Agent)
	}
	defaults, known := a.c.Agents[chosen]
	if a.c.Agents == nil {
		known = chosen == a.c.Agent
	}
	if !known {
		a.c.Log.Warn("start refused", "scope", r.Scope, "agent", chosen, "reason", "unknown agent")
		return 0, fmt.Errorf("core: start in %q: agent %q: %w", r.Scope, chosen, control.ErrUnknownAgent)
	}
	// An approved proposal's call names the effort the proposal showed:
	// one asked for, checked here when it was proposed, or the config's.
	if r.Effort != "" && !slices.Contains(control.Efforts, r.Effort) && a.caller.Via != control.ViaProposal {
		return 0, fmt.Errorf("core: start in %q: effort %q: %w", r.Scope, r.Effort, control.ErrUnknownEffort)
	}
	var scopeModel, scopeEffort, defaultModel, defaultEffort string
	if chosen == cmp.Or(sc.Agent, a.c.Agent) {
		scopeModel, scopeEffort = sc.Model, sc.Effort
	}
	if chosen == a.c.Agent {
		defaultModel, defaultEffort = a.c.Model, a.c.Effort
	}
	model := cmp.Or(r.Model, scopeModel, defaults.Model, defaultModel)
	effort := cmp.Or(r.Effort, scopeEffort, defaults.Effort, defaultEffort)
	if a.caller.Propose && !confined {
		// Nothing starts: a person decides (Caller.Propose).
		p := r
		p.Input = given
		return 0, a.proposeStart(ctx, p, chosen, model, effort)
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
	if confined {
		// A scope's confined threads share its directory, and one is open
		// at a time: none starts while another is open, or is being
		// reopened (ReopenThread takes the same lock).
		defer a.c.locks.lock(confinedLock(r.Scope))()
		other, err := openConfined(ctx, a.c.Store.DB, r.Scope, 0)
		if err != nil {
			return 0, err
		}
		if other != 0 {
			a.c.Log.Warn("start refused", "scope", r.Scope, "reason", "another confined thread of the scope is open", "open", int64(other))
			return 0, fmt.Errorf("core: start in %q: thread %d, a confined thread of the scope, is open: %w", r.Scope, other, session.ErrRefused)
		}
	}
	// Before anything is made: a stopped input opens no chat thread and
	// no workspace. A key accepted before, above, runs no hook again.
	var err error
	if r.Input, err = a.inputHooks(ctx, "Start", 0, r.Scope, r.Input); err != nil {
		return 0, err
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

	var (
		id        control.ThreadID
		sessionID int64
	)
	errDup := errors.New("the key was accepted before")
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
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
		// The first input came through the first link when the client that
		// opened it relayed the start; a start from elsewhere, such as the
		// supervisor's, came through no link.
		r.Input.Link = nil
		if client, _, ok := people.Split(a.caller.From); ok && client == link.Client {
			first := link
			r.Input.Link = &first
		}
		r.Input.Via = a.caller.Via
		// A confined thread's directory is known now, so a daemon that dies
		// before the launch recovers it in the same one.
		fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, From: a.caller.From, Key: r.Key, Text: r.Input.Text, Link: r.Input.Link,
			Before: r.Input.Before, After: r.Input.After, Via: r.Input.Via, Dir: dir})
		res, err = tx.ExecContext(ctx,
			`INSERT INTO sessions (thread_id, agent, state, model, effort, first_input, launch, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			thread, chosen, string(control.Creating), model, effort, string(fi), now, now)
		if err != nil {
			return fmt.Errorf("core: create session: %w", err)
		}
		sessionID, _ = res.LastInsertId()
		d := map[string]any{"thread": thread, "session": sessionID, "repo": r.Repo,
			"principal": string(a.caller.Principal), "model": model, "effort": effort}
		if a.caller.From != "" {
			d["from"] = a.caller.From
		}
		if a.caller.Via != "" {
			d["via"] = a.caller.Via // since protocol 3.26
		}
		data, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: id, Scope: r.Scope, Time: a.c.now(), Kind: "thread_created", Data: data})
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

	if err := a.c.launch(ctx, Launch{Session: sessionID, Thread: id, Agent: chosen,
		Model: model, Effort: effort, Principal: a.caller.Principal, From: a.caller.From, Key: r.Key, Input: r.Input,
		Confined: confined, Dir: dir}, r.Repo); err != nil {
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
// confined and is neither closed nor removed, and its state; else the
// newest one, closed or removed; 0 when there is none. A scope has one
// such thread open at a time, so the open one is its supervisor's also
// when it is an older one that was reopened (ReopenThread).
func (c *Core) LastConfined(ctx context.Context, scope string) (control.ThreadID, control.State, error) {
	var (
		id    int64
		state string
	)
	err := c.Store.DB.QueryRowContext(ctx, `SELECT id, state FROM threads WHERE scope = ? AND confined = 1
		ORDER BY state IN (?, ?), id DESC LIMIT 1`,
		scope, string(control.Closed), string(control.Removed)).Scan(&id, &state)
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

// SentTo returns the thread that a Send in scope took key for, and false
// when no Send took it. It is no operation of the control API: the
// supervisor's waker asks it about a wake's key from before the key named
// its thread (#443).
func (c *Core) SentTo(ctx context.Context, scope, key string) (control.ThreadID, bool, error) {
	var data []byte
	err := c.Store.DB.QueryRowContext(ctx, `SELECT data FROM inbox WHERE key = ?`, "send:"+scope+"\x00"+key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("core: send key: %w", err)
	}
	var d struct{ Thread control.ThreadID }
	if err := json.Unmarshal(data, &d); err != nil {
		return 0, false, fmt.Errorf("core: send key: %w", err)
	}
	return d.Thread, true, nil
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
// starts its agent; a confined session's directory, l.Dir, is always
// given by Workspaces.Confined. A failure is recorded as the session's end.
func (c *Core) launch(ctx context.Context, l Launch, repo string) error {
	var err error
	record := func(f FirstInput) error {
		f.Principal, f.From, f.Key, f.Text, f.Context, f.Link = l.Principal, l.From, l.Key, l.Input.Text, l.Context, l.Input.Link
		f.Before, f.After, f.Via = l.Input.Before, l.Input.After, l.Input.Via
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
	if l.Confined {
		// No clone: its directory, made empty only if it is not there yet.
		if l.Dir, err = c.Workspaces.Confined(ctx, l.Thread, l.Dir); err == nil {
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
		in := x.f.Input()
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
			x.l.placed, x.l.NoInput = x.f.Placed, x.f.NoInput
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
// chunk marked Reset; it ends when the new session has no terminal. A
// caller that may not read text (NoText) is refused: the terminal shows
// the prompts and the agent's output.
func (a *api) Terminal(ctx context.Context, id control.ThreadID, run string, after uint64) (<-chan control.TerminalChunk, error) {
	scope, err := a.checkThread(ctx, control.OpRead, "Terminal", id)
	if err != nil {
		return nil, err
	}
	if a.caller.NoText {
		return nil, a.refused("Terminal", scope, "the caller may not read text")
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
	return a.send(ctx, id, key, in, busy, nil)
}

// sendGuard is an approved interrupt's send: only while turn of session
// runs. What the interrupt did is set before the input is handed over:
// interrupted when it was sent, ended when that turn ended after the input
// was taken and before the interrupt, so that nothing was interrupted
// (#493). repeated is set when the send's key was taken already, by an
// earlier answer that took the input and recorded no outcome: nothing is
// taken or interrupted then, and the caller decides again (#523).
type sendGuard struct {
	session, turn                int64
	interrupted, ended, repeated bool
	// proposal makes it an approved proposal's send instead (#650): a
	// closed thread is ErrClosed, as for any send, and with question, the
	// handle of the question it was proposed to answer, it is sent only
	// while the thread waits on that question, else errGuardEnded.
	proposal bool
	question string
}

var errGuardEnded = errors.New("the requested turn ended")

func (a *api) send(ctx context.Context, id control.ThreadID, key string, in control.Input, busy control.Busy, guard *sendGuard) error {
	scope, err := a.actThread(ctx, control.OpSend, "Send", id)
	if err != nil {
		return err
	}
	if err := a.proposalKey("Send", scope, key); err != nil {
		return err
	}
	if a.caller.Propose {
		// Nothing is sent: a person decides (Caller.Propose).
		return a.proposeSend(ctx, scope, id, key, in)
	}
	asked := busy
	if a.caller.RefuseBusy {
		busy = control.Refuse
	}
	// An answer to an open question is not marked: the agent's question
	// tool takes it as the answer, line by line or as a choice's label.
	given := in
	in = a.marked(in)
	if key != "" {
		// One Send of a key at a time, so that the key's check below and the
		// transaction that takes it see the same.
		defer a.c.locks.lock("send:" + scope + "\x00" + key)()
	}
	if in, err = a.sendHooks(ctx, id, scope, key, in); err != nil {
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
	err = a.c.Store.Update(ctx, func(tx *store.Tx) error {
		var state control.State
		var err error
		sid, state, err = a.c.current(ctx, tx, id)
		if err != nil {
			return err
		}
		if (guard == nil || guard.proposal) && (state == control.Closed || state == control.Removed) {
			// Nothing is accepted: a message does not wake a closed thread.
			return fmt.Errorf("core: send to %v: %w", id, ErrClosed)
		}
		if in.Link, err = a.c.ownLink(ctx, tx, id, in.Link, a.caller.From); err != nil {
			return err
		}
		if key != "" {
			data, _ := json.Marshal(map[string]any{"thread": id, "session": sid, "text": in.Text})
			ok, err := tx.Accept(ctx, "send:"+scope+"\x00"+key, data)
			if err == nil && !ok && guard != nil {
				guard.repeated = true
			}
			if err != nil || !ok {
				return err // a repeated key is ignored, whatever Busy says
			}
		}
		if guard != nil && guard.proposal && guard.question != "" {
			// A proposed answer answers the question it was proposed for, or
			// nothing.
			var open string
			err := tx.QueryRowContext(ctx, `SELECT handle FROM requests WHERE session_id = ? AND state = 'open' ORDER BY id DESC LIMIT 1`, sid).Scan(&open)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if state != control.Waiting || open != guard.question {
				return errGuardEnded
			}
		}
		if guard != nil && !guard.proposal {
			var turn int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM turns WHERE session_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, sid).Scan(&turn)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if errors.Is(err, sql.ErrNoRows) || sid != guard.session || state != control.Running || turn != guard.turn {
				return errGuardEnded
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
			fi, _ := json.Marshal(FirstInput{Principal: a.caller.Principal, From: a.caller.From, Key: key, Text: in.Text, Wake: true, Link: in.Link,
				Before: in.Before, After: in.After, Via: a.caller.Via})
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ?, launch = 1 WHERE id = ?`, string(fi), sid); err != nil {
				return fmt.Errorf("core: wake: %w", err)
			}
			return nil
		}
		if state == control.Waiting && a.caller.NoAnswer {
			// The key is not taken: the transaction is rolled back.
			return fmt.Errorf("core: send to %v: waiting on a question: %w", id, ErrBusy)
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
					Key: key, Text: given.Text, Request: req, Link: in.Link, Via: a.caller.Via}); err != nil {
					return err
				}
				pending, err = pend(ctx, tx, sid, req, control.Answer{Text: given.Text})
				return err
			}
		}
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Send, Principal: a.caller.Principal, From: a.caller.From, Key: key,
			Text: in.Text, Link: in.Link, Before: in.Before, After: in.After, Via: a.caller.Via}); err != nil {
			return err
		}
		pending, err = pend(ctx, tx, sid, "", forAgent(in))
		return err
	})
	if err != nil || !accepted {
		return err
	}
	if answer != "" {
		return a.c.Runner.Answer(ctx, sid, pending, answer, control.Answer{Text: given.Text})
	}
	if wake {
		in.Via = a.caller.Via
		if err := a.c.resume(ctx, sid, a.caller.Principal, a.caller.From, key, in); err != nil {
			return fmt.Errorf("core: send to %v: %w", id, err)
		}
		return nil
	}
	if interrupt {
		// A guarded send names its turn: the runner interrupts only while
		// that turn runs, checked in the step that interrupts (#457), so a
		// turn that ended since the input was taken, and the turn after
		// it, are not interrupted (#493). The input was taken while that
		// turn ran, and is handed over as any input of a turn that ended
		// first: it starts the next one, or joins one that started.
		var turn int64
		if guard != nil {
			turn = guard.turn
		}
		err := a.c.Runner.Interrupt(ctx, sid, turn, session.Stopper{})
		switch {
		case guard != nil && errors.Is(err, session.ErrRefused):
			guard.ended = true
		case err != nil:
			return fmt.Errorf("core: send to %v: interrupt: %w", id, err)
		case guard != nil:
			guard.interrupted = true
		}
	}
	return a.c.Runner.Deliver(ctx, sid, pending, forAgent(in))
}

// forAgent is in as the agent gets it: the text the plugins added is in
// its Text.
func forAgent(in control.Input) control.Input {
	in.Text, in.Before, in.After = in.ForAgent(), "", ""
	return in
}

// sendHooks runs the input hook points for in, a Send of key to thread id
// in scope (inputHooks), unless the key was accepted before or the thread
// is closed: the Send then takes nothing, and a hook runs once for one
// accepted input, also when the input is given again after a restart.
// The caller holds the key's lock.
func (a *api) sendHooks(ctx context.Context, id control.ThreadID, scope, key string, in control.Input) (control.Input, error) {
	in.Before, in.After = "", ""
	if a.c.Hooks == nil {
		return in, nil
	}
	if key != "" {
		var n int
		if err := a.c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM inbox WHERE key = ?`, "send:"+scope+"\x00"+key).Scan(&n); err != nil {
			return in, fmt.Errorf("core: send key: %w", err)
		}
		if n > 0 {
			return in, nil
		}
	}
	sid, state, err := a.c.current(ctx, a.c.Store.DB, id)
	if err == nil && (state == control.Closed || state == control.Removed) {
		return in, nil
	}
	if err == nil && state == control.Waiting && !a.caller.NoAnswer {
		var open int
		if err := a.c.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE session_id = ? AND state = 'open'`, sid).Scan(&open); err != nil {
			return in, fmt.Errorf("core: request: %w", err)
		}
		if open > 0 {
			// It is the answer to the open question, which reaches the agent as
			// given: only the gate runs. Should the question close before the
			// input is taken, it goes as input without added text.
			return a.runInputHooks(ctx, "Send", id, scope, in, PointBeforeAgentRun)
		}
	}
	return a.inputHooks(ctx, "Send", id, scope, in)
}

func (a *api) Answer(ctx context.Context, req control.RequestID, ans control.Answer) error {
	if a.caller.Limit != nil && !slices.Contains(a.caller.Limit.Operations, "Answer") {
		// The request's scope is read for the log line only: the caller is
		// refused alike whether the request exists or not.
		return a.granted("Answer", a.c.requestScope(ctx, req))
	}
	if err := a.check(control.OpAnswer, "Answer", ""); err != nil {
		return err
	}
	var (
		scope    string
		thread   control.ThreadID
		sid      int64
		agentReq string
		state    string
	)
	err := a.c.Store.DB.QueryRowContext(ctx, `
		SELECT t.scope, t.id, r.session_id, r.request, r.state FROM requests r
		JOIN sessions s ON s.id = r.session_id JOIN threads t ON t.id = s.thread_id
		WHERE r.handle = ?`, string(req)).Scan(&scope, &thread, &sid, &agentReq, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return a.answerApproval(ctx, req, ans)
	}
	if err != nil {
		a.c.Log.Warn("control call refused", "op", "Answer", "principal", string(a.caller.Principal), "request", string(req), "reason", err)
		return fmt.Errorf("core: answer %q: %w", req, err)
	}
	if !slices.Contains(a.caller.Scopes, scope) {
		a.hidden("Answer", "request", string(req), scope)
		return noRequestErr(req)
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
		var err error
		if ans.Link, err = a.c.ownLink(ctx, tx, thread, ans.Link, a.caller.From); err != nil {
			return err
		}
		if err := a.c.Machine.ApplyTx(ctx, tx, sid, session.Input{Kind: session.Answer, Principal: a.caller.Principal, From: a.caller.From,
			Key: "answer:" + string(req), Text: ans.Text, Request: agentReq, Link: ans.Link, Via: a.caller.Via}); err != nil {
			return err
		}
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

// Stop interrupts the thread's running turn; with turn not 0, only while
// that turn is the running one. The runner checks the turn in the step that
// sends the interrupt, so a turn that ended after the caller looked, and
// the turn after it, are never stopped by the call.
func (a *api) Stop(ctx context.Context, id control.ThreadID, turn int64) error {
	if _, err := a.actThread(ctx, control.OpStop, "Stop", id); err != nil {
		return err
	}
	sid, state, err := a.c.current(ctx, a.c.Store.DB, id)
	if err != nil {
		return err
	}
	if state != control.Running {
		if turn != 0 {
			return fmt.Errorf("core: stop turn %d of thread %d: it is not running: %w", turn, id, session.ErrRefused)
		}
		return nil
	}
	// Who asked goes on the turn the runner interrupts, chosen in the same
	// step, so that the reader of that turn's end can tell it was stopped.
	return a.c.Runner.Interrupt(ctx, sid, turn, session.Stopper{Principal: a.caller.Principal, From: a.caller.From, Via: a.caller.Via})
}

// mayRead reports whether the caller may see e: its kind, when the caller
// is limited to some kinds, its scope, and every scope its data lists
// under "scopes" (an event about threads in several scopes, such as the
// end of a login outage). A layout event belongs to no scope, and every
// reader gets it.
func (a *api) mayRead(e control.Event) bool {
	if l := a.caller.Limit; l != nil && !slices.Contains(l.Events, e.Kind) {
		return false
	}
	if e.Kind == KindLayout && e.Thread == 0 && e.Scope == "" {
		return true // of no scope, and it names only a client (SetLayout)
	}
	if !slices.Contains(a.caller.Scopes, e.Scope) || !a.personSees(e.Scope) {
		return false
	}
	var d struct {
		Scopes []string `json:"scopes"`
	}
	if len(e.Data) > 0 {
		json.Unmarshal(e.Data, &d) // data that is not an object names no scopes
	}
	for _, s := range d.Scopes {
		if !slices.Contains(a.caller.Scopes, s) || !a.personSees(s) {
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
	if _, err := a.filterScope(ctx, "Events", &f); err != nil {
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

// checkHost allows HostTerminals, HostScreen and HostView: only when they are on, and
// only to the owner's caller with OpHost.
func (a *api) checkHost(name string) error {
	if err := a.granted(name, ""); err != nil {
		return err
	}
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
	list, err := a.c.HostTerms.List(ctx, owned)
	if err != nil {
		return nil, err
	}
	// A session a person started may be one of a thread's, opened again in
	// their own window (design, section 4.8): the row names that thread.
	for _, t := range list {
		r := t.Reported
		if r == nil {
			continue
		}
		var id int64
		err := a.c.Store.DB.QueryRowContext(ctx, `SELECT thread_id FROM sessions WHERE agent = ? AND agent_session_id = ?
			ORDER BY id DESC LIMIT 1`, r.Agent, r.Session).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("core: HostTerminals: %w", err)
		}
		// Named only to a caller that may read the thread, as Threads
		// would list it; another caller sees the window without it.
		tid := control.ThreadID(id)
		scope, err := a.c.scopeOf(ctx, a.c.Store.DB, tid)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if err == nil && a.caller.May.Has(control.OpRead) && slices.Contains(a.caller.Scopes, scope) && a.personSees(scope) {
			r.Thread = &tid
		}
	}
	return list, nil
}

// HostScreen reads one tmux pane's screen. A caller that may not read
// text (NoText) is refused every pane, before anything runs: a pane can
// show a session's text with none of hataori's processes in it, as after
// `hataori attach` ends.
func (a *api) HostScreen(ctx context.Context, pane string) (string, error) {
	var screen string
	err := a.hostRead(ctx, "HostScreen", pane, func(owned []int) (ok bool, err error) {
		screen, ok, err = a.c.HostTerms.Screen(ctx, pane, owned)
		return ok, err
	})
	if err != nil {
		return "", err
	}
	return screen, nil
}

// HostView reads one tmux pane's screen and its cursor, checked as
// HostScreen is.
func (a *api) HostView(ctx context.Context, pane string) (control.HostView, error) {
	var v control.HostView
	err := a.hostRead(ctx, "HostView", pane, func(owned []int) (ok bool, err error) {
		v, ok, err = a.c.HostTerms.View(ctx, pane, owned)
		return ok, err
	})
	if err != nil {
		return control.HostView{}, err
	}
	return v, nil
}

// hostRead makes the checks of a read of one pane around read, which
// reports ok false for a pane that HostTerminals would not list. A caller
// that may not read text is refused every pane, before any is read.
func (a *api) hostRead(ctx context.Context, name, pane string, read func(owned []int) (bool, error)) error {
	if err := a.checkHost(name); err != nil {
		return err
	}
	if a.caller.NoText {
		return a.refused(name, "", "the caller may not read text")
	}
	owned, err := a.c.hostPIDs(ctx)
	if err != nil {
		return err
	}
	ok, err := read(owned)
	if err == nil && !ok {
		err = fmt.Errorf("core: %s: tmux pane %q: %w", name, pane, ErrNotFound)
	}
	return err
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
		// A thread reopened with no conversation to resume is told what it
		// said before with its first input, which an attach does not have:
		// refused before anything changes, or the wake would fail it.
		var conversation string
		if err := tx.QueryRowContext(ctx, `SELECT agent_session_id FROM sessions WHERE id = ?`, sid).Scan(&conversation); err != nil {
			return fmt.Errorf("core: attach to thread %d: %w", number, err)
		}
		if conversation == "" {
			return fmt.Errorf("core: attach to thread %d: it has no conversation to resume; send it a message first, which starts one: %w",
				number, session.ErrRefused)
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
