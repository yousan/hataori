// Package runner runs agent sessions for the core: it starts each through
// a session host, follows what the host records and the files the codec
// asks for, feeds the state machine, and carries out what the codec says.
//
// Nothing is lost and nothing happens twice because a session's offsets are
// committed in the same transaction as the events read up to them. After a
// restart the runner reads on from the stored offsets; the codec is rebuilt
// by reading the lines before them again, with their results discarded.
// Events are numbered after the session's last stored sequence number, so
// the numbers go on across restarts.
//
// The runner names no agent. It reads an agent's terminal only for a codec
// whose Open asked to watch it (agent.Actions.WatchScreen), and only until
// the codec ends the watch (agent.Actions.StopScreen); otherwise the end of
// a terminal log is asked of the host only to be shown to a person.
package runner

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/holder/tmux"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Runner runs the sessions of one instance. It implements core.Runner.
type Runner struct {
	RevalidateInitial func(context.Context, *store.Tx, int64) error
	RevalidatePending func(context.Context, int64, int64) error
	AwayEnabled       bool
	AwayGrace         time.Duration
	Admit             func(context.Context, int64) error
	RevalidateLaunch  func(context.Context, int64) (bool, error)
	resumeLocks       sync.Map

	// heldMu, by state directory, serialises the start, the stop and the
	// letting go of a held window (startHeld, endHeld); heldSeen is the
	// last time a held window was seen running, so that tmux is not asked
	// at every poll.
	heldMu, heldSeen sync.Map

	Store    *store.Store
	Machine  *session.Machine
	Adapters map[string]agent.Adapter // by registered name
	Host     host.Command             // how to run `hataori host`
	Root     string                   // the instance root
	Log      *slog.Logger
	// IO resolves only a remotely placed session's durable machine/run. Local
	// sessions use the existing holder and a Local session I/O implementation.
	IO            func(context.Context, int64) (machineio.Session, error)
	Remotes       map[string]RemoteMachine
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}

	Instructions   string        // appended to every agent's own
	StartTimeout   time.Duration // for the agent to report its start; 60s when zero
	InterruptGrace time.Duration // after an interrupt, before the session's processes are stopped; 5s when zero
	Poll           time.Duration // how often the logs are read; 50ms when zero
	IdlePeriod     time.Duration // idle this long, a session's agent is stopped; 30 minutes when zero
	// BackgroundWait is how long the count of a turn that did not complete
	// holds (background); session.BackgroundWait when zero.
	BackgroundWait time.Duration
	// UsageDrain is usageDrain's bound; 2 s when zero.
	UsageDrain   time.Duration
	ContextBytes int // the cap on the earlier context a replacing session starts with; 8192 when zero
	// Cols and Rows are the size of the terminal a session's agent runs in,
	// for an agent that runs in one; the session host's 80×24 when zero.
	// Every launch takes them: a new session, a resumed one, one begun again.
	Cols, Rows uint16
	// Confine gives each launch of a session whose thread was started
	// confined its MCP servers, with what they need for that launch such as
	// a token, and instructions to append. It is called at every launch: a
	// new session, a resumed one, one begun again. Without it, or when it
	// fails, the launch fails: a confined session is never started
	// unconfined, and never with what another launch was given.
	Confine func(ctx context.Context, l ConfinedLaunch) (Confined, error)
	// Unconfine, when set, takes back what Confine gave the launches of a
	// session of a thread and of those it replaced, such as a token: Shut
	// calls it once the session is closed, before its agent is stopped. It
	// is called again when Shut is.
	Unconfine func(thread control.ThreadID, session int64)
	// ConfinedConfig is what the loaded config gives a launch, made now, of
	// a confined session of agent in scope, or false when it has none for
	// it. Each launch of a confined session takes its model and effort from
	// it, and a session that a daemon finds running with another one is put
	// to sleep at its first idle moment (follow). nil: a launch takes what
	// the session was started with, and nothing is compared.
	ConfinedConfig func(scope, agent string) (LaunchConfig, bool)
	// TrustFiles gives each launch the files of its scope that a start-up
	// dialog may name and the session may be let past it for
	// (agent.Spec.TrustFiles), for a thread of the scope on the repository
	// repo: the config's, and those the owner trusted from a blocked start
	// (core.Core.TrustedFiles). It is called at every launch; nil gives none.
	TrustFiles func(ctx context.Context, scope, repo string) []string
	// Tmux holds the tmux windows people started that were taken in
	// (TakeIn); a tmux.Holder running "tmux" from PATH when nil.
	Tmux *tmux.Holder
	// Holder is what holds the terminal of the agents it starts in one:
	// hataori's own host when empty, or HolderTmux. An agent on pipes is
	// always the own host's.
	Holder string

	mu   sync.Mutex
	live map[int64]*live

	startedMu sync.Mutex
	started   map[int64]bool // the sessions Start started in this process (claim)

	endingMu sync.Mutex
	ending   map[int64]chan struct{} // by session, closed as the stop of its earlier run returns (stopEarlier)

	point   func(name string) // for tests: called at the named points of a launch
	endWait time.Duration     // for tests: hostEndWait, when set
}

var _ core.Runner = (*Runner)(nil)

// live is a session the runner follows.
type live struct {
	catchingUp   bool
	catchupToken string
	id           int64
	dir          string
	adapter      agent.Adapter
	spec         agent.Spec
	codec        agent.Codec
	client       *host.Client // to the session host: the relay of hook calls, and the first sign that it answers
	started      time.Time
	ready        bool  // the session left creating; no start check any more
	resend       bool  // after a reconnect: send a first input that was not confirmed
	pending      bool  // after a reconnect: hand over pending input once the logs are read
	held         bool  // lv.mu is held from follow until the loop's first step has handed over the pending input
	upTo         int64 // the last pending input there was at the reconnect; later ones are handed over by their call
	// drain is until when the agent, its session asleep, is still read
	// before it is stopped: an agent may record a turn's last usage after
	// the turn's end (agent.Event.Late). Zero: not asleep by hataori's hand.
	drain  time.Time
	late   bool   // a late usage was read while draining
	failed bool   // the start timed out; the session is being stopped
	ended  bool   // the follower finished; delayed dispatch must not use its codec
	asleep bool   // put to sleep: its agent is being stopped, and nothing more is read
	screen bool   // the codec watches the terminal; until it says StopScreen
	seenTo uint64 // the position in the host's run up to which the codec was given its terminal
	// stale: the session was launched with another config than a launch
	// would be given now, and is put to sleep once it is idle (compare, set
	// once before the loop starts); staleAt is when that is tried next.
	stale   bool
	staleAt time.Time
	// taken is the window a person started that the session was taken in
	// from; nil for a session hataori's own host holds. Its input goes to
	// that window, and nothing ends the window's process.
	taken *takenIn

	answerPending int64      // pending answer whose effect is being journaled, under mu
	mu            sync.Mutex // the codec, the offsets and the actions, one at a time
	seq           uint64
	offset        int64            // in events.ndjson
	start         int64            // where the current agent's lines begin in events.ndjson
	files         map[string]int64 // followed files and their offsets
	stop          chan struct{}
	done          chan struct{}
	io            machineio.Session
	sources       map[string]machineio.Source
	readErr       error
	warmPending   bool
	remote        bool
	replayReplies []agent.HookReply
}

// StateDir is a session's state directory.
func StateDir(root string, id int64) string {
	return filepath.Join(root, "state", "sessions", strconv.FormatInt(id, 10))
}

func (r *Runner) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return 50 * time.Millisecond
}

func (r *Runner) startTimeout() time.Duration {
	if r.StartTimeout > 0 {
		return r.StartTimeout
	}
	return 60 * time.Second
}

func (r *Runner) idlePeriod() time.Duration {
	if r.IdlePeriod > 0 {
		return r.IdlePeriod
	}
	return 30 * time.Minute
}

func (r *Runner) backgroundWait() time.Duration {
	if r.BackgroundWait > 0 {
		return r.BackgroundWait
	}
	return session.BackgroundWait
}

// background returns how many background tasks the session's last turn left
// running, 0 without a turn. The count of a turn that
// completed holds until the next turn ends. That of a turn that failed, was
// stopped or met a usage limit holds only until the session reported nothing
// for backgroundWait: whether its tasks still run may never be learned, as
// when the turn that would say so fails too. stopped says that somebody
// stopped the turn (Stop), and that it ended by that.
func (r *Runner) background(ctx context.Context, tx *store.Tx, id int64) (n int, stopped bool, err error) {
	var outcome, reason, by string
	var updated int64
	err = tx.QueryRowContext(ctx, `SELECT t.background, t.status, t.reason, t.stopped_by, s.updated_at FROM turns t
		JOIN sessions s ON s.id = t.session_id WHERE t.session_id = ? ORDER BY t.id DESC LIMIT 1`, id).Scan(&n, &outcome, &reason, &by, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err == nil && outcome == session.Failure && r.activeAge(ctx, tx, id, time.UnixMilli(updated)) >= r.backgroundWait() {
		n = 0
	}
	return n, reason == session.ReasonInterrupted && by != "", err
}

// usageDrain bounds how long the agent of a session that hataori puts to
// sleep right after its turn is still read, for the usage it records after
// the turn's end.
func (r *Runner) usageDrain() time.Duration {
	if r.UsageDrain > 0 {
		return r.UsageDrain
	}
	return 2 * time.Second
}

func (r *Runner) interruptGrace() time.Duration {
	if r.InterruptGrace > 0 {
		return r.InterruptGrace
	}
	return 5 * time.Second
}

// firstInput is the input a session starts with. It is kept until a turn
// reports its id (firstInputID), so that a daemon that dies after
// sending it, or before, sends it (again) when it comes back.
type firstInput = core.FirstInput

type storedSpec struct {
	Agent string     `json:"agent"`
	Spec  agent.Spec `json:"spec"`
	// Config is what a confined session's running agent was launched with
	// (Runner.ConfinedConfig); nil when that was not recorded.
	Config *LaunchConfig `json:"config,omitempty"`
}

// ConfinedLaunch is one launch of a session whose thread was started
// confined, as Runner.Confine is told it.
type ConfinedLaunch struct {
	Session  int64
	Thread   control.ThreadID
	Scope    string
	StateDir string
	Dir      string // the session's working directory
}

// Confined is what Runner.Confine gives one launch.
type Confined struct {
	MCP          []agent.MCPServer // the session's only MCP servers
	Instructions string            // appended to the spec's
}

// confinement returns the thread and scope of session id, and whether its
// thread was started confined.
func (r *Runner) confinement(ctx context.Context, id int64) (control.ThreadID, string, bool, error) {
	var (
		thread   int64
		scope    string
		confined bool
	)
	err := r.Store.DB.QueryRowContext(ctx, `SELECT t.id, t.scope, t.confined FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE s.id = ?`, id).Scan(&thread, &scope, &confined)
	if err != nil {
		return 0, "", false, fmt.Errorf("runner: session %d: %w", id, err)
	}
	return control.ThreadID(thread), scope, confined, nil
}

// Start starts the agent of a new session through a session host. The
// first input is sent once the agent reports that it is ready.
func (r *Runner) Start(ctx context.Context, l core.Launch) (result error) {
	if r.Admit != nil {
		if err := r.endPriorPlacements(ctx, l.Session); err != nil {
			return err
		}
		if err := r.Admit(ctx, l.Session); err != nil {
			return err
		}
	}
	defer func() {
		if result != nil {
			r.startedMu.Lock()
			delete(r.started, l.Session)
			r.startedMu.Unlock()
		}
	}()
	// Once per session in this process: replace may start a session that
	// Recover finds to launch as well, when a follower of Reconnect
	// replaces one while Recover runs (#508).
	if !r.claim(l.Session) {
		r.Log.Info("the session is being started already", "session", l.Session)
		return nil
	}
	ad, ok := r.Adapters[l.Agent]
	if !ok {
		return fmt.Errorf("runner: no agent %q", l.Agent)
	}
	spec := agent.Spec{Dir: l.Dir, StateDir: StateDir(r.Root, l.Session), Model: l.Model, Effort: l.Effort,
		Instructions: r.Instructions, Approvals: agent.AllowAll}
	first := firstInput{AcceptedOperation: cmp.Or(l.AcceptedOperation, control.OpStart), Principal: l.Principal, From: l.From, Key: l.Key, Text: l.Input.Text, Context: l.Context, Link: l.Input.Link,
		Before: l.Input.Before, After: l.Input.After, Via: l.Input.Via, NoInput: l.NoInput}
	if remote, err := r.isRemote(ctx, l.Session); err != nil {
		return err
	} else if remote {
		spec, err := r.launchRemote(ctx, l.Session, l.Agent, spec, first)
		if err != nil {
			return err
		}
		return r.follow(l.Session, ad, spec, fresh)
	}
	spec, err := r.launch(ctx, l.Session, l.Agent, ad, spec, first)
	if err != nil {
		return err
	}
	return r.follow(l.Session, ad, spec, fresh)
}

// launch writes what the agent asks for into the session's state directory,
// stores the spec and the first input, and starts the session host. A
// session whose thread was started confined is launched confined, whatever
// spec says, or not at all. It returns the spec as stored, which the
// session's codec is made from.
func (r *Runner) launch(ctx context.Context, id int64, agentName string, ad agent.Adapter, spec agent.Spec, first firstInput) (agent.Spec, error) {
	dir := spec.StateDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	thread, scope, confined, err := r.confinement(ctx, id)
	if err != nil {
		return agent.Spec{}, err
	}
	if confined && spec.Confine == nil {
		spec.Confine = &agent.Confinement{}
	}
	var config *LaunchConfig
	if spec.Confine != nil && r.ConfinedConfig != nil {
		if c, ok := r.ConfinedConfig(scope, agentName); ok {
			config = &c
			spec.Model, spec.Effort = c.Model, c.Effort
		}
	}
	// The scope's config as it is at this launch, not as it was at the
	// session's first.
	spec.TrustFiles = nil
	var repo string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT repo, no_project_settings FROM threads WHERE id = ?`, int64(thread)).
		Scan(&repo, &spec.NoProjectSettings); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: session %d: the thread's repository: %w", id, err)
	}
	if r.TrustFiles != nil {
		spec.TrustFiles = r.TrustFiles(ctx, scope, repo)
	}
	// What the adapter gets: the spec, with what this launch alone is given.
	// The stored spec keeps the confinement and which MCP servers report,
	// not the servers, whose credentials hold for this launch only.
	run := spec
	if spec.Confine != nil {
		spec.Confine = &agent.Confinement{}
		if r.Confine == nil {
			return agent.Spec{}, fmt.Errorf("runner: session %d is confined, and nothing gives it its MCP servers; it was not started", id)
		}
		c, err := r.Confine(ctx, ConfinedLaunch{Session: id, Thread: thread, Scope: scope, StateDir: dir, Dir: spec.Dir})
		if err != nil {
			return agent.Spec{}, fmt.Errorf("runner: session %d: confinement: %w", id, err)
		}
		run.Confine = &agent.Confinement{MCP: c.MCP}
		for _, s := range c.MCP {
			if s.Reports {
				spec.Confine.Awaits = append(spec.Confine.Awaits, s.Name)
			}
		}
		if c.Instructions != "" {
			run.Instructions = strings.TrimSpace(run.Instructions + "\n\n" + c.Instructions)
		}
	}
	launch, err := ad.Launch(run)
	if err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	if run.Confine != nil && !launch.Confined {
		return agent.Spec{}, fmt.Errorf("runner: the agent %q cannot start a confined session; it was not started", agentName)
	}
	if run.Confine != nil && !launch.NoMentions {
		return agent.Spec{}, fmt.Errorf("runner: the agent %q cannot start a confined session: its input could name files to it; it was not started", agentName)
	}
	if run.Confine != nil && !launch.NoWriteTools {
		return agent.Spec{}, fmt.Errorf("runner: the agent %q cannot start a confined session: its own tools could write files; it was not started", agentName)
	}
	if run.Confine != nil && !launch.NoCommands {
		return agent.Spec{}, fmt.Errorf("runner: the agent %q cannot start a confined session: its input could be taken for a command; it was not started", agentName)
	}
	if run.NoProjectSettings && run.Confine == nil && !launch.NoProjectSettings {
		return agent.Spec{}, fmt.Errorf("runner: the agent %q cannot start without the repository's settings; it was not started", agentName)
	}
	for name, b := range launch.Files {
		if !filepath.IsLocal(name) {
			return agent.Spec{}, fmt.Errorf("runner: the agent asks for a file outside its state directory: %q", name)
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return agent.Spec{}, fmt.Errorf("runner: %w", err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return agent.Spec{}, fmt.Errorf("runner: %w", err)
		}
	}
	first.Dir = spec.Dir // so that a launch begun here can be begun again
	fb, _ := json.Marshal(first)
	var before, recorded int
	var model, effort, stored string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT launch, host_pid, model, effort, spec FROM sessions WHERE id = ?`, id).
		Scan(&before, &recorded, &model, &effort, &stored); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	if config != nil {
		var last storedSpec
		if stored != "" {
			if err := json.Unmarshal([]byte(stored), &last); err != nil {
				return agent.Spec{}, fmt.Errorf("runner: session %d: spec: %w", id, err)
			}
		}
		if before == 2 {
			// The host this launch may find running was started with what
			// was recorded then (hs.Again, below).
			config = last.Config
		} else {
			was := LaunchConfig{Model: last.Spec.Model, Effort: last.Spec.Effort}
			if last.Config != nil {
				was = *last.Config
			}
			// Not changed, what the agent reported stays: a resume may
			// report none.
			if config.Model != was.Model {
				model = config.Model
			}
			if config.Effort != was.Effort {
				effort = config.Effort
			}
		}
	}
	sb, _ := json.Marshal(storedSpec{Agent: agentName, Spec: spec, Config: config})
	if err := r.newAgent(ctx, id); err != nil {
		return agent.Spec{}, err
	}
	// launch = 2: the host is being started. Until it is recorded as
	// started, a daemon that comes back launches the session again, and
	// keeps the host it finds running instead of starting another.
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET spec = ?, first_input = ?, model = ?, effort = ?, launch = 2,
		context = CASE WHEN set_aside IS NULL AND ? != '' THEN ? ELSE context END WHERE id = ?`,
		string(sb), string(fb), model, effort, first.Context, first.Context, id); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	if r.Admit != nil {
		if err := r.Admit(ctx, id); err != nil {
			return agent.Spec{}, err
		}
	}
	r.at("before host start")
	hs := holder.Spec{Argv: launch.Argv, Env: launch.Env, Terminal: launch.Terminal, Dir: spec.Dir, Cols: r.Cols, Rows: r.Rows}
	if before == 2 {
		// A daemon that died had begun this launch: a host it started is
		// followed, not started again.
		hs.Again = &holder.Window{Holder: holder.HostName, Process: recorded}
	}
	start := r.hold().Start
	if r.Holder == HolderTmux && launch.Terminal {
		start = r.startHeld
	}
	w, err := start(ctx, dir, hs)
	if errors.Is(err, holder.ErrHeld) {
		return agent.Spec{}, fmt.Errorf("runner: session %d: another host still holds its state directory", id)
	}
	if err != nil {
		return agent.Spec{}, err
	}
	r.at("after host start")
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET host_pid = ?, launch = 0, io_error=CASE WHEN io_error IN (?,?) THEN '' ELSE io_error END WHERE id = ?`, w.Process, core.ErrPlacementWait.Error(), core.PlacementCanceledReason, id); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	return spec, nil
}

// newAgent retires input belonging to the previous agent before a new
// host can start. Unwritten messages fail visibly instead of reaching a
// later agent; written messages cannot be reported by the new process.
func (r *Runner) newAgent(ctx context.Context, id int64) error {
	err := r.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE pending_inputs SET stale = 1, context = '' WHERE session_id = ?`, id); err != nil {
			return err
		}
		if err := r.failStaleInput(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM pending_inputs WHERE session_id = ? AND request = '' AND written != 0`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	return nil
}

// failStaleInput also retires rows marked stale by an earlier runner.
// Failure and deletion commit together, before any reconnect can replay
// them. It leaves the session's current turn and conversation alone.
func (r *Runner) failStaleInput(ctx context.Context, tx *store.Tx, id int64) error {
	// An answer belongs to the old process's request and cannot be used
	// by its replacement. Do not leave it blocking thread closure.
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_inputs WHERE session_id = ? AND stale != 0 AND request != ''`, id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM pending_inputs WHERE session_id = ? AND stale != 0 AND request = '' AND written = 0 ORDER BY id`, id)
	if err != nil {
		return err
	}
	var inputs []string
	for rows.Next() {
		var row int64
		if err := rows.Scan(&row); err != nil {
			rows.Close()
			return err
		}
		inputs = append(inputs, strconv.FormatInt(row, 10))
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(inputs) == 0 {
		return err
	}
	var seq uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM sessions WHERE id = ?`, id).Scan(&seq); err != nil {
		return err
	}
	for _, input := range inputs {
		seq++
		e := agent.Event{Kind: agent.TurnEnded, Seq: seq, Input: input, Status: agent.Failed, Reason: agent.NotDelivered}
		if err := r.Machine.ApplyTx(ctx, tx, id, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
			return err
		}
		if err := clearReported(ctx, tx, id, []agent.Event{e}); err != nil {
			return err
		}
	}
	return nil
}

// own is hataori's own session host, the holder of every session's terminal.
func (r *Runner) own() *holder.Host { return &holder.Host{Command: r.Host, Poll: r.poll()} }

// hold is the holder of every session's terminal, for the holder's
// operations: start, input, screen, stop and alive.
func (r *Runner) hold() holder.Holder { return r.own() }

// window is the window of the session whose state directory is dir.
func (r *Runner) window(dir string) holder.Window { return r.own().Window(dir) }

// alive is what the holder says of the session whose state directory is dir.
func (r *Runner) alive(dir string) holder.Liveness {
	if remote, ok, err := r.remoteDir(dir); ok {
		if err != nil {
			return holder.Liveness{State: holder.Unknown}
		}
		l, _ := remote.Alive(context.Background())
		return l
	}
	if w, ok := heldOf(dir); ok {
		l, _ := r.tmuxHolder().Alive(context.Background(), w)
		if l.State == holder.Unknown {
			// tmux cannot tell, as when its socket file was removed under
			// a server that runs: the agent may run, and is never taken
			// for ended or for not there.
			l.State = holder.Running
		}
		if l.State == holder.Running {
			r.heldSeen.Store(dir, seen{ref: w.Ref, at: time.Now()})
		} else {
			r.heldSeen.Delete(dir)
		}
		return l
	}
	l, _ := r.hold().Alive(context.Background(), r.window(dir))
	return l
}

// polled is alive for the poll of a running session (step): a held window
// seen running less than heldPoll ago is taken to run still, without
// asking tmux. Whatever acts on an end asks alive itself.
func (r *Runner) polled(dir string) holder.Liveness {
	if w, ok := heldOf(dir); ok {
		if s, ok := r.heldSeen.Load(dir); ok && s.(seen).ref == w.Ref && time.Since(s.(seen).at) < heldPoll {
			return holder.Liveness{State: holder.Running}
		}
	}
	return r.alive(dir)
}

// stop stops the agent of the session whose state directory is dir. A held
// window is let go as well (endHeld).
func (r *Runner) stop(dir string, interrupt, terminate time.Duration) (holder.StopResult, error) {
	if remote, ok, err := r.remoteDir(dir); ok {
		if err != nil {
			return holder.StopResult{}, err
		}
		return remote.End(context.Background(), "end-"+remote.Target.Run, interrupt, terminate)
	}
	if w, ok := heldOf(dir); ok {
		ctx := context.Background()
		unlock := r.heldLock(dir)
		if now, ok := heldOf(dir); !ok || now.Ref != w.Ref {
			unlock()
			return r.stop(dir, interrupt, terminate) // let go meanwhile, or another started
		}
		defer unlock()
		res, err := r.tmuxHolder().Stop(ctx, w, interrupt, terminate)
		r.heldSeen.Delete(dir)
		r.warnLeft(dir, res)
		if err != nil {
			return res, err
		}
		r.endHeldLocked(ctx, dir, w)
		return res, nil
	}
	return r.hold().Stop(context.Background(), r.window(dir), interrupt, terminate)
}

// at calls the test hook for a point of a launch.
func (r *Runner) at(point string) {
	if r.point != nil {
		r.point(point)
	}
}

// Resume starts the agent of an asleep session again, resuming its
// conversation, and sends f's input once the agent reports that it is
// ready. A session whose conversation is set aside (sessions.set_aside) is
// started in a new conversation instead, and put to sleep when its turn
// ends (commit).
func (r *Runner) Resume(ctx context.Context, id int64, f core.FirstInput) error {
	f.Wake = true
	return r.resume(ctx, id, f)
}

// Wake starts the agent of an asleep session again, resuming its
// conversation, and gives it no input: the session becomes idle.
func (r *Runner) Wake(ctx context.Context, id int64, principal control.Principal) error {
	first := firstInput{AcceptedOperation: control.OpAttach, Principal: principal, Wake: true, NoInput: true}
	var raw string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT first_input FROM sessions WHERE id=?`, id).Scan(&raw); err != nil {
		return err
	}
	var accepted firstInput
	if err := json.Unmarshal([]byte(raw), &accepted); err != nil {
		return err
	}
	if accepted.Wake && accepted.NoInput {
		first.From, first.Via = accepted.From, accepted.Via
	}
	return r.resume(ctx, id, first)
}

// hostEndWait bounds how long a wake waits for the host of the session's
// earlier run to record its end. Once its agent is gone a host drains the
// output and lets its followers finish, each for 2 s at most unless a
// descriptor of the agent's lives on elsewhere (internal/host). It is short
// because a daemon started again recovers its wakes one at a time before it
// serves.
func (r *Runner) hostEndWait() time.Duration {
	if r.endWait > 0 {
		return r.endWait
	}
	return 10 * time.Second
}

// waitEnded waits, for d at most, until the holder says the agent of the
// session whose state directory is dir has ended. It reports whether it
// has.
func (r *Runner) waitEnded(ctx context.Context, dir string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !r.alive(dir).Ended() {
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.poll()):
		}
	}
	return true
}

// stopEarlier stops the agent of an earlier run of session id, whose state
// directory is dir, when it runs. It returns a channel that is closed once
// that stop has returned. A run has one stop: one under way is not begun
// again, since a stop that is late reaches the agent started meanwhile.
func (r *Runner) stopEarlier(id int64, dir string) <-chan struct{} {
	r.endingMu.Lock()
	defer r.endingMu.Unlock()
	if c, ok := r.ending[id]; ok {
		return c
	}
	stopped := make(chan struct{})
	if r.alive(dir).State != holder.Running {
		close(stopped)
		return stopped
	}
	if r.ending == nil {
		r.ending = map[int64]chan struct{}{}
	}
	r.ending[id] = stopped
	go func() {
		r.at("stop-earlier")
		if _, err := r.stop(dir, time.Second, 2*time.Second); err != nil {
			r.Log.Warn("stopping the agent of an earlier run", "session", id, "err", err)
		}
		r.endingMu.Lock()
		delete(r.ending, id)
		r.endingMu.Unlock()
		close(stopped)
	}()
	return stopped
}

// endEarlier returns once the earlier run of session id, whose state
// directory is dir, has ended: a wake was accepted and not begun, so what
// runs there is the agent that was put to sleep, or closed. One that runs
// is stopped: a daemon that stopped while that agent was still read after
// its sleep (usageDrain), or before the stop reached its host, has not
// stopped it, and stopSleepers does not see it: the session is creating
// (#713). Either way its host records the end a little after the
// agent is gone (it drains the output and lets its followers finish), and
// the end is waited for, hostEndWait at most (#549).
func (r *Runner) endEarlier(ctx context.Context, id int64, dir string) error {
	stopped := r.stopEarlier(id, dir)
	deadline := time.After(r.hostEndWait())
	ended := r.waitEnded(ctx, dir, r.hostEndWait())
	if ended {
		// A stop that still ran could reach the agent about to be started.
		select {
		case <-stopped:
			return nil
		case <-deadline:
		case <-ctx.Done():
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ended {
		return fmt.Errorf("runner: session %d: stopping its earlier agent has not ended after %v", id, r.hostEndWait())
	}
	return fmt.Errorf("runner: session %d: its host is still running after %v", id, r.hostEndWait())
}

func (r *Runner) resume(ctx context.Context, id int64, first firstInput) error {
	lock, _ := r.resumeLocks.LoadOrStore(id, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if r.Admit != nil {
		var launch int
		if err := r.Store.DB.QueryRowContext(ctx, `SELECT launch FROM sessions WHERE id=?`, id).Scan(&launch); err != nil {
			return err
		}
		if launch == 0 {
			return nil
		}
	}
	if r.Admit != nil {
		if err := r.Admit(ctx, id); err != nil {
			return err
		}
	}
	if remote, err := r.isRemote(ctx, id); err != nil {
		return err
	} else if remote {
		return r.resumeRemote(ctx, id, first)
	}
	// The agent that was put to sleep must be gone, and read to its end,
	// before another starts in the same state directory.
	r.mu.Lock()
	old := r.live[id]
	r.mu.Unlock()
	if old != nil {
		select {
		case <-old.done:
		case <-time.After(time.Minute):
			return fmt.Errorf("runner: session %d: the agent put to sleep has not ended", id)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var specJSON, resumeID string
	var launching int
	var aside sql.NullString
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT spec, agent_session_id, launch, set_aside FROM sessions WHERE id = ?`, id).
		Scan(&specJSON, &resumeID, &launching, &aside); err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	var s storedSpec
	if err := json.Unmarshal([]byte(specJSON), &s); err != nil {
		return fmt.Errorf("runner: session %d: spec: %w", id, err)
	}
	ad, ok := r.Adapters[s.Agent]
	if !ok {
		return fmt.Errorf("runner: no agent %q", s.Agent)
	}
	switch {
	case aside.Valid:
		// Its conversation is set aside (sessions.set_aside): the input gets
		// one of its own, told nothing of what the thread said before.
		resumeID = ""
	case resumeID == "" && first.NoInput:
		// A new conversation is told what the thread said before with its
		// first input, and this wake has none.
		return fmt.Errorf("runner: session %d has no conversation to resume; a message starts a new one", id)
	case resumeID == "" && launching != 2:
		// No conversation to resume: a reopen found none recorded, or made
		// the thread's directory again (core's ReopenThread). A new one
		// starts in the directory, told what the thread said before, as a
		// session that replaces one the agent lost.
		thread, _, _, err := r.confinement(ctx, id)
		var earlier string
		if err == nil {
			earlier, err = r.earlier(ctx, r.Store.DB, thread)
		}
		if err != nil {
			r.Log.Error("the thread's earlier context", "session", id, "err", err) // the new conversation starts without it
		}
		first.Context = earlier + first.Context
	}
	if t, ok := takenOf(s.Spec.StateDir); ok {
		// Left by a take-in that ended another way: the new agent's input
		// must not go to the window. Before the earlier run is ended, as
		// its relay host goes after the window's record.
		if err := r.letGo(ctx, id, s.Spec.StateDir, t, nil); err != nil {
			return fmt.Errorf("runner: session %d: %w", id, err)
		}
	}
	// launch = 2: a daemon that died had begun this launch, after the old
	// host was gone; a host running now is the new one.
	if launching != 2 {
		if err := r.endEarlier(ctx, id, s.Spec.StateDir); err != nil {
			return err
		}
		// A held window whose end a daemon that died did not see through:
		// its relay host and window go before another start.
		if w, ok := heldOf(s.Spec.StateDir); ok {
			r.endHeld(ctx, s.Spec.StateDir, w)
		}
		// An ended host records its end before it lets go of the state
		// directory.
		w := r.window(s.Spec.StateDir)
		if !r.own().WaitReleased(w, 5*time.Second) {
			return fmt.Errorf("runner: session %d: its ended host still holds the state directory", id)
		}
		if err := r.skipRest(ctx, id, s.Spec.StateDir); err != nil {
			return err
		}
		// The status of the ended host would make the new one look ended.
		if err := r.own().Reset(w); err != nil {
			return fmt.Errorf("runner: %w", err)
		}
	}
	// What a conversation of one input's own left behind, in case the
	// daemon ended it without the machine doing so.
	if _, err := r.Store.DB.ExecContext(ctx, `DELETE FROM session_files WHERE session_id = ? AND aside = 1`, id); err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	spec := s.Spec
	spec.ResumeID = resumeID // "" for a new conversation
	// A session taken in keeps the model its window last reported, as the
	// take-in stored it; the options the person started it with are not
	// known, and are not reused.
	spec.TakenIn = false // resumed, it is launched by hataori's own host
	spec, err := r.launch(ctx, id, s.Agent, ad, spec, first)
	if err != nil {
		return err
	}
	return r.follow(id, ad, spec, wake)
}

// skipRest moves the session's offsets to the end of what its last agent
// wrote: what it wrote while it was being put to sleep is not read.
func (r *Runner) skipRest(ctx context.Context, id int64, dir string) error {
	end := func(p string) int64 {
		b, _ := os.ReadFile(p)
		return complete(b, 0)
	}
	return r.Store.Update(ctx, func(tx *store.Tx) error {
		n := end(filepath.Join(dir, host.EventsFile))
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET log_offset = ?, log_start = ?, screen_offset=0 WHERE id = ?`, n, n, id); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT path FROM session_files WHERE session_id = ?`, id)
		if err != nil {
			return err
		}
		var paths []string
		for rows.Next() {
			var p string
			rows.Scan(&p)
			paths = append(paths, p)
		}
		rows.Close()
		for _, p := range paths {
			n := end(p)
			if _, err := tx.ExecContext(ctx, `UPDATE session_files SET offset = ?, start = ? WHERE session_id = ? AND path = ?`,
				n, n, id, p); err != nil {
				return err
			}
		}
		// The ranges read so far are the last agent's; the next one's codec
		// is given none of them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_reads WHERE session_id = ?`, id); err != nil {
			return err
		}
		return nil
	})
}

// Reconnect follows every session that was live when the daemon stopped.
// Their agents kept running under their hosts. An asleep session whose
// agent is still running, because the daemon stopped while putting it to
// sleep, has its agent stopped. One launched with another config, and idle,
// is put to sleep.
func (r *Runner) Reconnect(ctx context.Context) error {
	if err := r.observeMachines(ctx); err != nil {
		return err
	}
	r.startRemoteCleanup()
	if err := r.stopSleepers(ctx); err != nil {
		return err
	}
	// Only completed launches (zero) can reconnect. A canceled placement
	// intent keeps a distinct durable phase, including across later restarts.
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT id, spec FROM sessions
		WHERE state IN ('creating', 'idle', 'running', 'waiting') AND spec != '' AND launch = 0`)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	type row struct {
		id   int64
		spec string
	}
	var found []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.spec); err != nil {
			rows.Close()
			return err
		}
		found = append(found, x)
	}
	rows.Close()
	for _, x := range found {
		if r.RevalidateLaunch != nil {
			allowed, err := r.RevalidateLaunch(ctx, x.id)
			if err != nil {
				return err
			}
			if !allowed {
				continue
			}
		}
		var s storedSpec
		if err := json.Unmarshal([]byte(x.spec), &s); err != nil {
			r.Log.Error("session spec unreadable", "session", x.id, "err", err)
			continue
		}
		ad, ok := r.Adapters[s.Agent]
		if !ok {
			r.Log.Error("session of an agent not registered", "session", x.id, "agent", s.Agent)
			continue
		}
		if err := r.follow(x.id, ad, s.Spec, reconnect); err != nil {
			r.Log.Error("reconnect", "session", x.id, "err", err)
		}
	}
	r.settle(ctx)
	return nil
}

// How follow starts following a session.
type mode int

const (
	fresh     mode = iota // a new session: nothing was read yet
	reconnect             // after a daemon restart: the agent kept running
	wake                  // a new agent of a session read before: read on from the stored offsets
)

// stopSleepers stops the agents of asleep, closed and failed sessions that are
// still running, as after a daemon that stopped while it put one to sleep
// or closed it.
func (r *Runner) stopSleepers(ctx context.Context) error {
	// A failed session's agent may still run too: one failed at a start-up
	// dialog is stopped just after its failure is stored (#423).
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT id FROM sessions WHERE state IN ('asleep', 'closed', 'failed') AND spec != ''`)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		remote, err := r.isRemote(ctx, id)
		if err != nil {
			return err
		}
		if remote {
			r.cleanupRemoteSleeper(ctx, id)
			continue
		}
		dir := StateDir(r.Root, id)
		if t, ok := takenOf(dir); ok {
			// A window taken in whose release or close the daemon did not
			// finish: its hooks report again, and its relay host goes.
			if err := r.letGo(ctx, id, dir, t, nil); err != nil {
				r.Log.Error("letting a window taken in go", "session", id, "err", err)
			}
			continue
		}
		if w, ok := heldOf(dir); ok && r.alive(dir).State != holder.Running {
			// A held window whose end the daemon that died did not see
			// through: its relay host and the window's note go.
			r.endHeld(ctx, dir, w)
			continue
		}
		if r.alive(dir).State != holder.Running {
			continue
		}
		r.Log.Warn("stopping the agent of an asleep, closed or failed session", "session", id)
		r.stopEarlier(id, dir)
	}
	return nil
}

// follow starts following a session. On reconnect, the codec first reads
// what was read before, with its results discarded, to rebuild its state.
// A codec that watches the screen is given its terminal from the first
// byte of the host's current run: what the agent it follows wrote, and
// nothing an earlier run left in the log (#505, #520).
func (r *Runner) follow(id int64, ad agent.Adapter, spec agent.Spec, m mode) error {
	if err := r.Store.Update(context.Background(), func(tx *store.Tx) error {
		return r.failStaleInput(context.Background(), tx, id)
	}); err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	resumed := m == reconnect
	lv := &live{id: id, dir: spec.StateDir, adapter: ad, codec: ad.NewCodec(spec), spec: spec,
		files: map[string]int64{}, stop: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
	if err := r.openIO(lv); err != nil {
		return err
	}
	if !lv.remote {
		if t, ok := takenOf(lv.dir); ok {
			lv.taken = &t
		}
	}
	var created int64
	err := r.Store.DB.QueryRow(`SELECT last_seq, log_offset, log_start, created_at, screen_offset FROM sessions WHERE id = ?`, id).
		Scan(&lv.seq, &lv.offset, &lv.start, &created, &lv.seenTo)
	if err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	if m != reconnect {
		lv.seenTo = 0
	}
	if err := r.compare(lv); err != nil {
		return err
	}
	if resumed {
		lv.started = time.UnixMilli(created)
		lv.resend = true
		lv.pending = true
		r.Store.DB.QueryRow(`SELECT coalesce(max(id), 0) FROM pending_inputs WHERE session_id = ?`, id).Scan(&lv.upTo)
	}
	if m != fresh {
		rows, err := r.Store.DB.Query(`SELECT path, offset FROM session_files WHERE session_id = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			var o int64
			rows.Scan(&p, &o)
			lv.files[p] = o
		}
		rows.Close()
	}
	var replies []agent.HookReply
	if resumed && r.beginCatchup(lv) {
		replies = r.warm(lv)
		if lv.remote && !lv.catchingUp && lv.readErr == nil {
			if err := r.reconcileMachineActions(lv); err != nil {
				r.pauseIO(lv, err)
				lv.warmPending = true
			}
		}
		// Before any new line is read: input written before the restart,
		// so that the report of it, if it comes, is known for what it is.
		if lv.readErr == nil {
			r.handOverWritten(lv)
		}
	}
	if !lv.remote {
		if c, err := host.Dial(lv.dir); err == nil {
			lv.client = c
		}
	}
	if resumed && (lv.client != nil || lv.remote) && lv.readErr == nil {
		// Replies decided before the daemon died and not sent: send those
		// whose call still waits. The host refuses the others anyway.
		lv.replayReplies = replies
	}
	if !resumed || lv.taken != nil && r.creating(id) {
		// A take-in is ready once its codec is opened: one the daemon died
		// before that opens now, and watches no screen.
		lv.mu.Lock()
		a := lv.codec.Open()
		// Only a new start: a codec rebuilt after a restart does not watch
		// the screen, so no key is sent from a replay of it.
		lv.screen = a.WatchScreen
		r.do(lv, a)
		lv.mu.Unlock()
	}
	if lv.pending {
		// Input accepted after the reconnect waits until what was pending
		// before it is handed over: the loop's first step unlocks.
		lv.mu.Lock()
		lv.held = true
	}
	r.mu.Lock()
	if r.live == nil {
		r.live = map[int64]*live{}
	}
	r.live[id] = lv
	r.mu.Unlock()
	go r.loop(lv)
	return nil
}

// handOverPending hands the session's pending input and answers to its
// agent, oldest first: those the core accepted and a daemon that died did
// not hand over, or whose input the agent had not reported. lv.mu is held.
func (r *Runner) handOverPending(lv *live) {
	rows, err := r.Store.DB.Query(`SELECT id, request, data, context FROM pending_inputs
		WHERE session_id = ? AND id <= ? AND written = 0 AND stale = 0 ORDER BY id`, lv.id, lv.upTo)
	if err != nil {
		r.Log.Error("pending input", "session", lv.id, "err", err)
		return
	}
	type row struct {
		id                 int64
		req, data, earlier string
	}
	var found []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.req, &x.data, &x.earlier); err != nil {
			r.Log.Error("pending input", "session", lv.id, "err", err)
			break
		}
		found = append(found, x)
	}
	rows.Close()
	for _, x := range found {
		var err error
		if x.req != "" {
			var a control.Answer
			if err = json.Unmarshal([]byte(x.data), &a); err == nil {
				err = r.handOver(lv, x.id, true, func() (agent.Actions, error) {
					return lv.codec.Answer(agent.Answer{Request: x.req, Text: a.Text, Choices: a.Choices, Allow: a.Allow})
				})
			}
		} else {
			var in control.Input
			if err = json.Unmarshal([]byte(x.data), &in); err == nil {
				err = r.handOver(lv, x.id, false, func() (agent.Actions, error) {
					return lv.codec.Input(agent.Input{ID: strconv.FormatInt(x.id, 10), Text: x.earlier + in.Text})
				})
			}
		}
		if err != nil {
			r.Log.Error("pending input", "session", lv.id, "pending", x.id, "err", err)
		}
	}
}

// handOverWritten tells the codec of a reconnected session which input was
// written to the agent and not reported yet: the first input and pending
// input marked written. The codec records them and writes nothing.
func (r *Runner) handOverWritten(lv *live) {
	var fi string
	r.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = ?`, lv.id).Scan(&fi)
	var f firstInput
	if fi != "" && json.Unmarshal([]byte(fi), &f) == nil && f.Sent && f.Written {
		r.do(lv, r.inputActions(lv, agent.Input{ID: firstInputID, Text: f.Context + f.Input().ForAgent(), Written: true, WrittenAt: r.writtenClock(lv, f.WrittenAt), WrittenText: f.WrittenText, Earlier: !f.Current}))
		lv.resend = false
	}
	rows, err := r.Store.DB.Query(`SELECT id, data, context, written, written_text, written_at FROM pending_inputs
		WHERE session_id = ? AND request = '' AND written != 0 ORDER BY id`, lv.id)
	if err != nil {
		r.Log.Error("written input", "session", lv.id, "err", err)
		return
	}
	var ins []agent.Input
	for rows.Next() {
		var id, written, writtenAt int64
		var data, earlier string
		var writtenText *string
		var in control.Input
		if rows.Scan(&id, &data, &earlier, &written, &writtenText, &writtenAt) == nil && json.Unmarshal([]byte(data), &in) == nil {
			// One that a runner from before agent.Input.Earlier wrote, maybe
			// to this agent, also one that it wrote to an agent before and
			// marked stale; this runner removes those at a launch (newAgent).
			ins = append(ins, agent.Input{ID: strconv.FormatInt(id, 10), Text: earlier + in.Text, Written: true, WrittenAt: time.UnixMilli(writtenAt), WrittenText: writtenText, Earlier: written != writtenNow})
		}
	}
	rows.Close()
	for _, in := range ins {
		in.WrittenAt = r.writtenClock(lv, in.WrittenAt.UnixMilli())
		r.do(lv, r.inputActions(lv, in))
	}
}

// inputActions is what the codec makes of in; an error is logged.
func (r *Runner) inputActions(lv *live, in agent.Input) agent.Actions {
	a, err := lv.codec.Input(in)
	if err != nil {
		r.Log.Error("input", "session", lv.id, "input", in.ID, "err", err)
	}
	return a
}

// markWritten records that the inputs ids are about to be written to the
// agent: from now on a restart tells the codec instead of writing them
// again. unmarkWritten records that nothing of them was.
func (r *Runner) markWritten(lv *live, ids []string, text map[string]string) error {
	return r.setWritten(lv, ids, true, text)
}

func (r *Runner) unmarkWritten(lv *live, ids []string) { r.setWritten(lv, ids, false, nil) }

// writtenNow is pending_inputs.written of an input that a runner that knows
// agent.Input.Earlier, this one or a later one, wrote to the agent that runs
// (one written to an agent before is removed: newAgent). 1 is of one that a
// runner from before it wrote, maybe to the agent that runs: agent.Input.Earlier.
const writtenNow = 2

func (r *Runner) setWritten(lv *live, ids []string, written bool, text map[string]string) error {
	err := r.Store.Update(context.Background(), func(tx *store.Tx) error {
		return r.setWrittenTx(context.Background(), tx, lv, ids, written, text)
	})
	if err != nil {
		r.Log.Error("marking input written", "session", lv.id, "err", err)
	}
	return err
}

func (r *Runner) setWrittenTx(ctx context.Context, tx *store.Tx, lv *live, ids []string, written bool, text map[string]string) error {
	for _, id := range ids {
		var writtenText *string
		if t, ok := text[id]; written && ok {
			writtenText = &t
		}
		if id == firstInputID {
			var fi string
			if err := tx.QueryRowContext(ctx, `SELECT first_input FROM sessions WHERE id = ?`, lv.id).Scan(&fi); err != nil {
				return err
			}
			var f firstInput
			if fi == "" || json.Unmarshal([]byte(fi), &f) != nil {
				continue
			}
			f.Written, f.Current, f.WrittenText = written, written, writtenText
			if written && f.WrittenAt == 0 {
				f.WrittenAt = time.Now().UnixMilli()
			}
			if !written {
				f.WrittenAt = 0
			}
			b, _ := json.Marshal(f)
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ? WHERE id = ?`, string(b), lv.id); err != nil {
				return err
			}
			continue
		}
		row, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue // not one of ours
		}
		mark := 0
		if written {
			mark = writtenNow
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pending_inputs SET written = ?, written_text = ?, written_at=CASE WHEN ?=0 THEN 0 WHEN written_at=0 THEN ? ELSE written_at END WHERE id = ? AND session_id = ?`, mark, writtenText, mark, time.Now().UnixMilli(), row, lv.id); err != nil {
			return err
		}
	}
	return nil
}

// handOver carries out what the codec makes of one pending input or
// answer. Input stays pending until the agent reports the turn it starts
// (see commit): a codec may hold it until the agent can take it, and one
// told that it was not handed over (Actions.KeysFailed) says with its turn's
// end what became of it, so that is no error. An answer is removed once the
// host has taken it. One the codec refuses is removed, and its error
// returned. lv.mu is held.
func (r *Runner) handOver(lv *live, pending int64, answer bool, f func() (agent.Actions, error)) error {
	if r.RevalidatePending != nil {
		if err := r.RevalidatePending(context.Background(), lv.id, pending); err != nil {
			data, _ := json.Marshal(map[string]any{"action": "pending input", "state": "refused", "reason": "authorization or scope changed; queued delivery was withdrawn"})
			recordErr := r.Store.Update(context.Background(), func(tx *store.Tx) error {
				var id control.ThreadID
				if e := tx.QueryRowContext(context.Background(), `SELECT thread_id FROM sessions WHERE id=?`, lv.id).Scan(&id); e != nil {
					return e
				}
				if !answer {
					var seq uint64
					if e := tx.QueryRowContext(context.Background(), `SELECT last_seq+1 FROM sessions WHERE id=?`, lv.id).Scan(&seq); e != nil {
						return e
					}
					if e := r.Machine.ApplyTx(context.Background(), tx, lv.id, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.TurnEnded, Seq: seq, Input: strconv.FormatInt(pending, 10), Status: agent.Failed, Reason: agent.NotDelivered}}); e != nil {
						return e
					}
				}
				_, e := tx.Append(context.Background(), control.Event{Thread: id, Kind: "machine_action", Data: data})
				return e
			})
			if recordErr != nil {
				return errors.Join(err, recordErr)
			}
			r.Store.DB.QueryRow(`SELECT last_seq FROM sessions WHERE id=?`, lv.id).Scan(&lv.seq)
			r.dropPending(pending)
			return err
		}
	}
	a, err := f()
	if err != nil && answer {
		return errors.Join(err, r.refusePendingAnswer(lv.id, pending))
	}
	if err == nil {
		if answer {
			lv.answerPending = pending
			defer func() { lv.answerPending = 0 }()
		}
		if err = r.do(lv, a); err != nil && !answer && a.KeysFailed != nil {
			return nil
		}
		if err != nil || !answer {
			return err
		}
	}
	r.dropPending(pending)
	return err
}

// dropPending removes a pending input that cannot be handed over.
func (r *Runner) dropPending(pending int64) {
	if _, err := r.Store.DB.Exec(`DELETE FROM pending_inputs WHERE id = ?`, pending); err != nil {
		r.Log.Error("pending input", "pending", pending, "err", err)
	}
}

// saveRead stores how far a step read, and the ranges it read after the
// session's earlier ones, in the transaction that stores its events.
func saveRead(ctx context.Context, tx *store.Tx, session int64, b batch) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET log_offset = ?,screen_offset=? WHERE id = ?`, b.offset, b.screenOffset, session); err != nil {
		return err
	}
	for p, o := range b.files {
		// A file first followed for a conversation of one input's own goes
		// when that conversation ends (session.Machine).
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_files (session_id, path, offset, aside)
			SELECT ?, ?, ?, set_aside IS NOT NULL FROM sessions WHERE id = ?
			ON CONFLICT (session_id, path) DO UPDATE SET offset = excluded.offset`, session, p, o, session); err != nil {
			return err
		}
	}
	reads := b.reads
	if len(reads) == 0 {
		return nil
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(n), 0) FROM session_reads WHERE session_id = ?`, session).Scan(&n); err != nil {
		return err
	}
	for _, rd := range reads {
		n++
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_reads (session_id, n, path, start, end) VALUES (?, ?, ?, ?, ?)`,
			session, n, rd.path, rd.start, rd.end); err != nil {
			return err
		}
		if err := saveProof(ctx, tx, session, n, rd); err != nil {
			return err
		}
	}
	return nil
}

// warm feeds the codec what was read before the stored offsets, in the
// order the steps gave it (session_reads). Lines that no stored range
// covers, read before ranges were stored, come first: the host's log, then
// each followed file, each from where the current agent's lines begin in
// it, as they were replayed then. What is before that is an earlier
// agent's, or was skipped while the session was put to sleep, and is not
// given.
// It returns the replies to hook calls it produced: one that was never sent
// because the daemon died first still has its call waiting at the host.
func (r *Runner) warm(lv *live) []agent.HookReply {
	replies, err := r.warmIO(lv)
	if err != nil {
		r.pauseIO(lv, err)
		lv.warmPending = true
		return nil
	}
	r.pauseIO(lv, nil)
	return replies
}

// storedReads returns where the current agent's lines begin in each
// followed file, and the session's stored ranges in order, only from where
// the current agent's lines begin and none past the stored offsets.
func (r *Runner) storedReads(lv *live) (map[string]int64, []readRange, error) {
	starts := map[string]int64{"": lv.start}
	rows, err := r.Store.DB.Query(`SELECT path, start FROM session_files WHERE session_id = ?`, lv.id)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var p string
		var s int64
		if err := rows.Scan(&p, &s); err != nil {
			rows.Close()
			return nil, nil, err
		}
		starts[p] = s
	}
	rows.Close()
	rows, err = r.Store.DB.Query(`SELECT r.path, r.start, r.end, coalesce(p.digest, ''), coalesce(p.handle, ''), coalesce(p.generation, '') FROM session_reads r LEFT JOIN session_read_proofs p ON p.session_id=r.session_id AND p.n=r.n WHERE r.session_id = ? ORDER BY r.n`, lv.id)
	if err != nil {
		return starts, nil, err // the starts still hold without the ranges
	}
	defer rows.Close()
	var out []readRange
	for rows.Next() {
		var rd readRange
		if err := rows.Scan(&rd.path, &rd.start, &rd.end, &rd.digest, &rd.source.Handle, &rd.source.Generation); err != nil {
			return starts, nil, err
		}
		rd.source.Name = rd.path
		limit, ok := lv.offset, rd.path == ""
		if rd.path == "@screen" {
			limit, ok = int64(lv.seenTo), true
		}
		if !ok {
			limit, ok = lv.files[rd.path]
		}
		if !ok || rd.start < starts[rd.path] || rd.end > limit {
			continue
		}
		out = append(out, rd)
	}
	return starts, out, rows.Err()
}

// source tells a hook call, as the host records it, from the agent's
// own output.
func source(line []byte) agent.Source {
	if bytes.HasPrefix(line, []byte(`{"hook":`)) {
		return agent.Hook
	}
	return agent.Output
}

// lines returns the complete lines of b between from and to. A last line
// without its newline is left for later.
func lines(b []byte, from, to int64) [][]byte {
	if to > int64(len(b)) {
		to = int64(len(b))
	}
	if from >= to {
		return nil
	}
	var out [][]byte
	for _, l := range bytes.SplitAfter(b[from:to], []byte("\n")) {
		if len(l) == 0 || l[len(l)-1] != '\n' {
			break
		}
		out = append(out, bytes.TrimSuffix(l, []byte("\n")))
	}
	return out
}

// complete returns the offset after the last newline of b, from from on.
func complete(b []byte, from int64) int64 {
	if from >= int64(len(b)) {
		return from
	}
	i := bytes.LastIndexByte(b[from:], '\n')
	if i < 0 {
		return from
	}
	return from + int64(i) + 1
}

// Close stops following all sessions; their agents go on running.
func (r *Runner) Close() {
	r.mu.Lock()
	lives := r.live
	cancel, done := r.cleanupCancel, r.cleanupDone
	r.cleanupCancel, r.cleanupDone = nil, nil
	r.live = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	for _, lv := range lives {
		close(lv.stop)
		<-lv.done
		if lv.client != nil {
			lv.client.Close()
		}
	}
}

func (r *Runner) get(id int64) (*live, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lv := r.live[id]
	if lv == nil {
		return nil, fmt.Errorf("runner: session %d is not running", id)
	}
	return lv, nil
}

func (r *Runner) loop(lv *live) {
	defer close(lv.done)
	// Held from follow: unlock once the pending input is handed over, which
	// a failed commit puts off to the next poll.
	for lv.held {
		ok := r.stepLocked(lv)
		if ok && lv.pending {
			select {
			case <-lv.stop:
			case <-time.After(r.poll()):
				continue
			}
		}
		lv.held = false
		lv.mu.Unlock()
		if !ok {
			r.mu.Lock()
			delete(r.live, lv.id)
			r.mu.Unlock()
			return
		}
	}
	tick := time.NewTicker(r.poll())
	defer tick.Stop()
	lastTick, lastWindow := time.Now(), time.Now()
	for {
		select {
		case <-lv.stop:
			return
		case <-tick.C:
		}
		if !r.step(lv) {
			r.mu.Lock()
			delete(r.live, lv.id)
			r.mu.Unlock()
			return
		}
		if time.Since(lastTick) >= time.Second && lv.readErr == nil {
			if !r.sessionChecked(lv.id) || lv.catchingUp {
				continue
			}
			lastTick = time.Now()
			lv.mu.Lock()
			r.do(lv, r.tickCodec(lv))
			lv.mu.Unlock()
			r.checkIdle(lv)
		}
		if lv.taken != nil && time.Since(lastWindow) >= windowCheck {
			lastWindow = time.Now()
			r.checkWindow(lv)
		}
	}
}

// batch is what one read of the logs produced.
type batch struct {
	events       []agent.Event
	actions      []agent.Actions
	offset       int64
	files        map[string]int64
	reads        []readRange // what was given to the codec, in order
	err          error
	screenOffset uint64
}

// readRange is complete lines of the host's log (path "") or of a followed
// file, from start to end, given to the codec at once. The ranges a step
// read are stored with its events, so that a codec rebuilt later is given
// the lines in the same order (warm).
type readRange struct {
	path       string
	start, end int64
	source     machineio.Source
	digest     string
}

// step reads what is new, commits it, and carries out the codec's actions.
// It returns false when the session's agent has ended and all it recorded
// is read.
func (r *Runner) step(lv *live) bool {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	return r.stepLocked(lv)
}

// stepLocked is step with lv.mu held.
func (r *Runner) stepLocked(lv *live) (following bool) {
	defer func() {
		if !following {
			lv.ended = true
		}
	}()
	if !r.beginCatchup(lv) {
		return true
	}
	if lv.asleep {
		return !r.polled(lv.dir).Ended()
	}
	if lv.warmPending {
		lv.codec = lv.adapter.NewCodec(lv.spec)
		lv.replayReplies = r.warm(lv)
		if lv.readErr != nil {
			if r.confirmedGone(lv) {
				r.exit(lv)
				r.finishSessionCatchup(lv)
				return false
			}
			return true
		}
		if lv.remote && !lv.catchingUp {
			if err := r.reconcileMachineActions(lv); err != nil {
				r.pauseIO(lv, err)
				return true
			}
		}
		lv.warmPending = false
		lv.resend = true
		r.handOverWritten(lv)
	}
	exited := r.polled(lv.dir).Ended() // looked at before reading, so nothing written before the exit is missed
	b := r.read(lv)
	if b.err != nil {
		if r.confirmedGone(lv) {
			r.exit(lv)
			r.finishSessionCatchup(lv)
			return false
		}
		r.pauseIO(lv, b.err)
		r.noteRemoteReadFailure(lv)
		lv.warmPending = true
		return true
	}
	r.watchScreen(lv, &b)
	if b.err != nil {
		r.pauseIO(lv, b.err)
		lv.warmPending = true
		return true
	}
	first, err := r.commit(lv, b)
	if err != nil {
		// The codec has read lines whose results were not stored: build it
		// again from what was, so the lines are read once more by a codec
		// that has not seen them.
		r.Log.Error("commit", "session", lv.id, "err", err)
		lv.codec = lv.adapter.NewCodec(lv.spec)
		lv.screen = false // the new codec is not opened: it does not watch
		r.warm(lv)
		// What the old codec knew of input: what was written, and what it
		// held to write later, the first input among it.
		lv.resend = true
		r.handOverWritten(lv)
		lv.pending = true
		r.Store.DB.QueryRow(`SELECT coalesce(max(id), 0) FROM pending_inputs WHERE session_id = ?`, lv.id).Scan(&lv.upTo)
		return true
	}
	lv.offset = b.offset
	lv.seenTo = b.screenOffset
	r.pauseIO(lv, nil)
	for p, o := range b.files {
		lv.files[p] = o
	}
	for _, e := range b.events {
		if e.Kind == agent.SessionEnded && lv.taken != nil && e.Reason != agent.WrongSession && e.Reason != clearReason {
			// The agent in the window ended: the window closed.
			go r.windowClosed(lv)
			continue
		}
		if e.Kind == agent.SessionEnded && e.Reason == agent.WrongSession {
			// The session failed; its agent runs another conversation.
			go r.stop(lv.dir, time.Second, 2*time.Second)
		}
		if e.Kind == agent.SessionEnded && e.Reason == agent.BlockedAtStart {
			// The session failed at a start-up dialog it may not answer;
			// its agent still shows it.
			go r.stop(lv.dir, time.Second, 2*time.Second)
		}
	}
	if lv.catchingUp {
		if err := r.reconcileMachineActions(lv); err != nil {
			r.pauseIO(lv, err)
			return true
		}
		if exited {
			r.exit(lv)
			r.finishSessionCatchup(lv)
			return false
		}
		for _, a := range b.actions {
			lv.replayReplies = append(lv.replayReplies, a.Reply...)
		}
		if !r.finishSessionCatchup(lv) {
			return true
		}
		// Catch-up did not hand a first/pending input over. Reconsider those
		// only in a fresh read step after every owned session is reconciled.
		lv.pending = true
		r.Store.DB.QueryRow(`SELECT coalesce(max(id),0) FROM pending_inputs WHERE session_id=?`, lv.id).Scan(&lv.upTo)
		return true
	}
	for _, a := range b.actions {
		r.act(lv, a)
	}
	if len(lv.replayReplies) > 0 {
		r.replayReplies(lv)
	}
	if first != nil {
		a, err := lv.codec.Input(agent.Input{ID: firstInputID, Text: first.Context + first.Input().ForAgent()})
		if err != nil {
			r.Log.Error("first input", "session", lv.id, "err", err)
		}
		r.do(lv, a)
	}
	if lv.pending {
		// What the agent reported before the daemon died is committed now,
		// so only what it did not report is handed over again.
		lv.pending = false
		r.handOverPending(lv)
	}
	if exited && len(b.events) == 0 {
		r.exit(lv)
		return false
	}
	if lv.failed && lv.client == nil && !lv.remote {
		return false // never had a host: nothing more will come
	}
	r.checkStart(lv)
	r.checkStale(lv, len(b.events) > 0)
	if !lv.drain.IsZero() && !lv.asleep {
		// Its agent was put to sleep and is still read: stop it now, or
		// once the turn's late usage was read.
		for _, e := range b.events {
			if e.Kind == agent.Usage && e.Late {
				lv.late = true
			}
		}
		if lv.late || r.activeAge(context.Background(), r.Store.DB, lv.id, lv.drain.Add(-r.usageDrain())) >= r.usageDrain() {
			lv.asleep = true
			go r.stop(lv.dir, time.Second, 2*time.Second)
		}
	}
	return true
}

// read decodes what is new in the files the codec follows, then in the
// host's log of hook calls and output, then in the files those calls named
// and anything new in the others. An agent writes a line of its transcript
// before it reports what follows it, such as a turn's last message before
// the turn's end, so a line written to a followed file before a hook call is
// decoded first even when both are new in one step. The log is read before
// the files and decoded after them, so that this holds for what is written
// meanwhile too.
func (r *Runner) read(lv *live) batch {
	b := batch{offset: lv.offset, screenOffset: lv.seenTo, files: map[string]int64{}}
	for p, off := range lv.files {
		b.files[p] = off
	}
	readFiles := func() {
		for i := 0; i < len(b.files)+1; i++ { // a followed file may name another
			found := false
			for p, off := range b.files {
				ls, end, proof, err := r.readLines(lv, p, off)
				if err != nil {
					b.err = err
					return
				}
				if end == off {
					continue
				}
				found = true
				b.reads = append(b.reads, proof)
				for _, l := range ls {
					r.decode(lv, &b, agent.File, l)
				}
				b.files[p] = end
			}
			if !found {
				break
			}
		}
	}
	calls, end, proof, err := r.readLines(lv, "", lv.offset)
	if err != nil {
		b.err = err
		return b
	}
	readFiles()
	if b.err != nil {
		return b
	}
	if end > lv.offset {
		b.reads = append(b.reads, proof)
		for _, l := range calls {
			r.decode(lv, &b, source(l), l)
		}
		b.offset = end
	}
	readFiles()
	return b
}

// newLines returns the complete lines of the file path behind offset off,
// and the offset after them; off when there are none. A last line without
// its newline is left for later. Only what is behind off is read, and a
// file no longer than off is not opened.
func newLines(path string, off int64) ([][]byte, int64) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= off {
		return nil, off
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, off
	}
	defer f.Close()
	data := make([]byte, st.Size()-off)
	n, err := f.ReadAt(data, off)
	if err != nil && err != io.EOF { // EOF: it became shorter meanwhile
		return nil, off
	}
	end := complete(data[:n], 0)
	return lines(data[:n], 0, end), off + end
}

// watchScreen gives the codec what the agent wrote to its terminal since
// the last step, while it watches the screen. Once the codec has said
// StopScreen, the terminal is not read again.
func (r *Runner) watchScreen(lv *live, b *batch) {
	if !lv.screen {
		return
	}
	if lv.remote {
		r.watchRemoteScreen(lv, b)
		return
	}
	since := func(from uint64) ([]byte, uint64) { return r.own().Since(r.window(lv.dir), from) }
	if _, ok := heldOf(lv.dir); ok {
		since = func(from uint64) ([]byte, uint64) { return heldScreen(lv.dir, from) }
	}
	data, next := since(lv.seenTo)
	b.screenOffset = next
	if len(data) > 0 {
		r.decode(lv, b, agent.Screen, data)
	}
}

// decode runs one line through the codec and keeps what it gave.
func (r *Runner) decode(lv *live, b *batch, src agent.Source, l []byte) {
	a := lv.codec.Decode(src, l)
	if a.StopScreen {
		lv.screen = false
	}
	for _, e := range a.Events {
		if e.Kind == agent.Unknown {
			r.Log.Warn("skipped a line the codec does not know", "session", lv.id, "line", truncate(e.Text, 200))
			continue
		}
		b.events = append(b.events, e)
	}
	for _, p := range a.Follow {
		if _, ok := b.files[p]; !ok {
			if _, ok := lv.files[p]; !ok {
				b.files[p] = 0
			}
		}
	}
	a.Events, a.Follow = nil, nil
	b.actions = append(b.actions, a)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// commit writes the events and the offsets they were read up to in one
// transaction, and the first input once the agent is ready.
func (r *Runner) commit(lv *live, b batch) (*firstInput, error) {
	var (
		first *firstInput
		rep   *replacement
		slept bool
	)
	seq := lv.seq
	err := r.Store.Update(context.Background(), func(tx *store.Tx) error {
		ctx := context.Background()
		seq = lv.seq
		first, rep, slept = nil, nil, false
		for _, e := range b.events {
			seq++
			e.Seq = seq
			if err := r.Machine.ApplyTx(ctx, tx, lv.id, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
				if !errors.Is(err, session.ErrRefused) {
					return err
				}
				r.Log.Warn("event refused by the state machine", "session", lv.id, "err", err)
				if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seq = ? WHERE id = ?`, seq, lv.id); err != nil {
					return err
				}
			}
		}
		// A session whose agent no longer has the conversation is replaced
		// in the transaction that stores its failure, so a daemon that stops
		// after it still replaces it once (#527). A window taken in is not:
		// its agent ending means the window closed.
		if lv.taken == nil && cannotResume(b.events) {
			var err error
			if rep, err = r.replaceTx(ctx, tx, lv); err != nil {
				return fmt.Errorf("replacing a session that cannot resume: %w", err)
			}
		}
		rb := b
		if !lv.drain.IsZero() {
			rb.files = nil // the rows of the conversation that ended are gone
		}
		if err := saveRead(ctx, tx, lv.id, rb); err != nil {
			return err
		}
		if err := r.firstInputStep(ctx, tx, lv, b, &first); err != nil {
			return err
		}
		if err := r.endAside(ctx, tx, lv, b, &slept); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT last_seq FROM sessions WHERE id=?`, lv.id).Scan(&seq)
	})
	if err == nil {
		lv.seq = seq
		if !lv.catchingUp {
			lv.resend = false
		}
		if slept {
			r.Log.Info("the conversation of one input ended with its turn; the session sleeps", "session", lv.id)
			lv.drain = time.Now().Add(r.usageDrain())
		}
		if rep != nil {
			r.at("after cannot resume")
			go r.launchReplacement(*rep)
		}
	}
	return first, err
}

// firstInputStep hands over the session's first input once its agent is
// ready, and clears it once the agent reported its turn.
func (r *Runner) firstInputStep(ctx context.Context, tx *store.Tx, lv *live, b batch, first **firstInput) error {
	// The first input goes once the agent is ready.
	var state, fi string
	if err := tx.QueryRowContext(ctx, `SELECT state, first_input FROM sessions WHERE id = ?`, lv.id).Scan(&state, &fi); err != nil {
		return err
	}
	if err := clearReported(ctx, tx, lv.id, b.events); err != nil {
		return err
	}
	if fi == "" {
		return nil
	}
	var f firstInput
	if err := json.Unmarshal([]byte(fi), &f); err != nil {
		return err
	}
	if lv.catchingUp && !(f.Sent && (reported(b.events, firstInputID) || notDelivered(b.events, firstInputID))) {
		return nil
	}
	willHandOver := !f.NoInput && !f.Written && ((!f.Sent && control.State(state) == control.Idle) || (f.Sent && lv.resend && !reported(b.events, firstInputID) && !notDelivered(b.events, firstInputID)))
	if willHandOver && r.RevalidateInitial != nil {
		if err := r.RevalidateInitial(ctx, tx, lv.id); err != nil {
			if errors.Is(err, core.ErrForbidden) {
				return nil
			}
			return err
		}
	}
	switch {
	case f.NoInput && (control.State(state) == control.Idle || control.State(state) == control.Running || control.State(state) == control.Waiting):
		// Woken with nothing to say: ready is all it waited for, and a turn
		// typed at the terminal may be read with it. What its agent was to be
		// told before an input waits for the next one.
		if f.Context != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context = ? WHERE id = ?`, f.Context, lv.id); err != nil {
				return err
			}
		}
		f = firstInput{}
	case f.NoInput:
		return nil
	case !f.Sent && control.State(state) == control.Idle:
		if err := r.Machine.ApplyTx(ctx, tx, lv.id, session.Input{Kind: session.Send, Principal: f.Principal, From: f.From, Key: f.Key, Text: f.Text, Link: f.Link,
			Before: f.Before, After: f.After, Via: f.Via}); err != nil {
			return err
		}
		f.Sent = true
		if err := tx.QueryRowContext(ctx, `SELECT id FROM turns WHERE session_id=? AND ended_at IS NULL`, lv.id).Scan(&f.TurnID); err != nil {
			return err
		}
		*first = &f
	case f.Sent && reported(b.events, firstInputID):
		// The agent reported the turn: delivered, and with it what the
		// agent was still to be told, except by an input that has a
		// conversation of its own.
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context = '' WHERE id = ? AND set_aside IS NULL`, lv.id); err != nil {
			return err
		}
		f = firstInput{}
	case f.Sent && notDelivered(b.events, firstInputID):
		// The agent never reported it, or it was stopped before it was
		// written; its turn failed, as the events say.
		// Older launches may have kept the owed context only in this
		// first input. Preserve it before discarding an undelivered call.
		if f.Context != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context = ? WHERE id = ? AND set_aside IS NULL`, f.Context, lv.id); err != nil {
				return err
			}
		}
		f = firstInput{}
	case f.Sent && lv.resend:
		// The daemon died between starting the turn and a report of it:
		// hand it to the codec again.
		*first = &f
	default:
		return nil
	}
	fb := ""
	if f.Sent {
		b, _ := json.Marshal(f)
		fb = string(b)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET first_input = ? WHERE id = ?`, fb, lv.id); err != nil {
		return err
	}
	return nil
}

// endAside puts a session to sleep that ran one input in a conversation of
// its own (sessions.set_aside), once that input's turn has ended. It does
// so in the transaction that ends the turn, and the core gives no input to
// a session in such a conversation (core.Core.send), so none reaches it
// after its turn; what the turn left running in the background ends with
// it. The machine gives the session back the conversation it set aside.
func (r *Runner) endAside(ctx context.Context, tx *store.Tx, lv *live, b batch, slept *bool) error {
	if len(b.events) == 0 {
		return nil
	}
	var (
		state, fi string
		aside     sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `SELECT state, first_input, set_aside FROM sessions WHERE id = ?`, lv.id).Scan(&state, &fi, &aside); err != nil {
		return err
	}
	if !aside.Valid || control.State(state) != control.Idle || fi != "" {
		return nil // its own conversation, or the input's turn has not ended
	}
	if err := r.Machine.ApplyTx(ctx, tx, lv.id, session.Input{Kind: session.IdleTimeout}); err != nil {
		return err
	}
	*slept = true
	return nil
}

// cannotResume reports whether evs end the session because its agent no
// longer has the conversation.
func cannotResume(evs []agent.Event) bool {
	for _, e := range evs {
		if e.Kind == agent.SessionEnded && e.Reason == agent.CannotResume {
			return true
		}
	}
	return false
}

// clearReported removes the pending input whose turn the agent reported, or
// whose turn ended without one (unreported):
// the codec names it by the Input.ID it was given, the row's id.
func clearReported(ctx context.Context, tx *store.Tx, id int64, evs []agent.Event) error {
	for _, e := range evs {
		gone := e.Kind == agent.TurnStarted || e.Kind == agent.InputTaken || unreported(e)
		if !gone || e.Input == "" {
			continue
		}
		row, err := strconv.ParseInt(e.Input, 10, 64)
		if err != nil {
			continue // not one of ours
		}
		if e.Kind != agent.TurnEnded {
			// The agent got what it was given before its text, if anything.
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context = '' WHERE id = ?1
				AND EXISTS (SELECT 1 FROM pending_inputs WHERE id = ?2 AND session_id = ?1 AND context != '')`, id, row); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_inputs WHERE id = ? AND session_id = ? AND request = ''`, row, id); err != nil {
			return err
		}
	}
	return nil
}

// firstInputID is the Input.ID of a session's first input. It is not a
// number, so it names no pending input.
const firstInputID = "first"

// unreported reports whether e ends a turn whose input the agent did not
// report and will not: it never did, or the input was stopped before it was
// written.
func unreported(e agent.Event) bool {
	return e.Kind == agent.TurnEnded && !e.RetainInput && (e.Reason == agent.NotDelivered || e.Reason == agent.NotTaken || e.Status == agent.Interrupted)
}

// notDelivered reports whether a turn among evs ended so for the input id.
func notDelivered(evs []agent.Event, id string) bool {
	return slices.ContainsFunc(evs, func(e agent.Event) bool { return unreported(e) && e.Input == id })
}

// reported reports whether a turn among evs reports the input id.
func reported(evs []agent.Event, id string) bool {
	for _, e := range evs {
		if (e.Kind == agent.TurnStarted || e.Kind == agent.InputTaken) && e.Input == id {
			return true
		}
	}
	return false
}

// do commits the events of actions that did not come from a line, then
// carries out the rest. It returns the first error writing to the host.
func (r *Runner) do(lv *live, a agent.Actions) error {
	if a.StopScreen {
		lv.screen = false
	}
	if len(a.Events) > 0 {
		b := batch{offset: lv.offset, screenOffset: lv.seenTo, files: map[string]int64{}}
		for p, o := range lv.files {
			b.files[p] = o
		}
		for _, e := range a.Events {
			if e.Kind == agent.Unknown {
				r.Log.Warn("the codec reports", "session", lv.id, "text", truncate(e.Text, 200))
				continue
			}
			b.events = append(b.events, e)
		}
		if len(b.events) > 0 {
			if _, err := r.commit(lv, b); err != nil {
				r.Log.Error("commit", "session", lv.id, "err", err)
			}
		}
	}
	for _, p := range a.Follow {
		if _, ok := lv.files[p]; !ok {
			lv.files[p] = 0
		}
	}
	return r.act(lv, a)
}

// act hands input to the agent through its holder and answers waiting hook
// calls. It returns the first error, which it has logged.
func (r *Runner) act(lv *live, a agent.Actions) error {
	if len(a.Keys) == 0 && len(a.Write) == 0 && len(a.Reply) == 0 {
		return nil
	}
	// failed tells the codec that its keys were not handed over.
	failed := func(nothing bool) {
		if nothing && len(a.Inputs) > 0 && (lv.remote || a.KeysFailed != nil) {
			r.unmarkWritten(lv, a.Inputs)
		}
		if a.KeysFailed != nil {
			a.KeysFailed(nothing)
		}
	}
	if len(a.Inputs) > 0 {
		if err := r.markWritten(lv, a.Inputs, a.InputText); err != nil {
			// Not written: a codec that is not told so, which counts it
			// written, fails its turn as "not delivered" when no report comes.
			failed(true)
			a.Keys, a.Write = nil, nil
			if len(a.Reply) == 0 {
				return err
			}
		}
		r.at("input marked")
	}
	defer r.at("input written")
	if lv.remote {
		return r.actRemote(lv, a, failed)
	}
	if lv.client == nil {
		c, err := host.Dial(lv.dir)
		if err != nil {
			r.Log.Error("host unreachable", "session", lv.id, "err", err)
			failed(true)
			return &holder.NotSent{Err: err, Refused: lv.taken != nil}
		}
		lv.client = c
	}
	var first error
	ctx, w := context.Background(), r.window(lv.dir)
	if len(a.Keys) > 0 {
		in := func() error { return r.hold().Input(ctx, w, a.Keys) }
		if hw, ok := heldOf(lv.dir); ok {
			in = func() error { return r.tmuxHolder().Input(ctx, hw, a.Keys) }
		}
		if lv.taken != nil {
			// Only to the agent that was taken in, while it is the window's
			// foreground process: its holder looks.
			in = func() error { return r.tmuxHolder().Input(ctx, lv.taken.Window, a.Keys) }
		}
		if err := in(); err != nil {
			var no *holder.NotSent
			level := slog.LevelError
			if errors.As(err, &no) && no.Refused {
				level = slog.LevelWarn // the window may take it later
			}
			r.Log.Log(ctx, level, "input", "session", lv.id, "err", err)
			first = err
			failed(no != nil)
		}
	}
	if len(a.Write) > 0 {
		// An agent on pipes: hataori's own host is its only holder.
		if err := r.own().Write(ctx, w, a.Write); err != nil {
			r.Log.Error("input", "session", lv.id, "err", err)
			first = cmp.Or(first, err)
		}
	}
	for _, rep := range a.Reply {
		if err := lv.client.Answer(rep.Call, rep.Body); err != nil {
			r.Log.Error("answer", "session", lv.id, "call", rep.Call, "err", err)
			first = cmp.Or(first, err)
		}
	}
	return first
}

// exit records that the agent's process ended.
func (r *Runner) exit(lv *live) {
	if lv.taken != nil {
		// The relay host ended: nothing points at the window any more.
		if err := r.letGo(context.Background(), lv.id, lv.dir, *lv.taken, nil); err != nil {
			r.Log.Error("letting a window taken in go", "session", lv.id, "err", err)
		}
	}
	st := r.alive(lv.dir).Status
	if w, ok := heldOf(lv.dir); ok {
		// How it ended is read; its window and relay host go now.
		defer r.endHeld(context.Background(), lv.dir, w)
	}
	in := session.Input{Kind: session.Exit}
	switch {
	case st == nil:
		in.Reason = session.ReasonInterrupted // the host died with it, as in a reboot (section 4.5)
	case st.Stopped:
		in.Reason = "stopped"
	case st.Shutdown:
		in.Reason = session.ReasonInterrupted // the host got SIGTERM: a shutdown, as in a reboot (section 4.5)
	}
	if in.Reason == session.ReasonInterrupted {
		// The host's end, not a Stop, ends the open turn, also one a person
		// asked to stop that had not ended yet: it names nobody (#542).
		if _, err := r.Store.DB.Exec(`UPDATE turns SET stopped_by = '' WHERE session_id = ? AND ended_at IS NULL`, lv.id); err != nil {
			r.Log.Error("exit: forget who stopped the turn", "session", lv.id, "err", err)
		}
	}
	if err := r.Machine.Apply(context.Background(), lv.id, in); err != nil && !errors.Is(err, session.ErrRefused) {
		r.Log.Error("exit", "session", lv.id, "err", err)
	}
}

// checkIdle puts the session to sleep when it has been idle for the idle
// period, with no background task running (background) and no open request,
// and stops its agent. The session keeps its id, so its next input resumes it.
func (r *Runner) checkIdle(lv *live) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if !r.sessionChecked(lv.id) || lv.catchingUp || lv.asleep || lv.failed || lv.taken != nil { // a window taken in runs as its person keeps it
		return
	}
	ctx := context.Background()
	var state string
	var updated int64
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT state, updated_at FROM sessions WHERE id = ?`, lv.id).Scan(&state, &updated); err != nil ||
		control.State(state) != control.Idle || r.activeAge(ctx, r.Store.DB, lv.id, time.UnixMilli(updated)) < r.idlePeriod() {
		return
	}
	if r.attached(lv) {
		return
	}
	slept := false
	err := r.Store.Update(ctx, func(tx *store.Tx) error {
		slept = false
		var state string
		var updated int64
		if err := tx.QueryRowContext(ctx, `SELECT state, updated_at FROM sessions WHERE id = ?`, lv.id).Scan(&state, &updated); err != nil {
			return err
		}
		if control.State(state) != control.Idle || r.activeAge(ctx, tx, lv.id, time.UnixMilli(updated)) < r.idlePeriod() {
			return nil
		}
		var open int
		background, _, err := r.background(ctx, tx, lv.id)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE session_id = ? AND state = 'open'`, lv.id).Scan(&open); err != nil {
			return err
		}
		if background > 0 || open > 0 {
			return nil
		}
		if err := r.Machine.ApplyTx(ctx, tx, lv.id, session.Input{Kind: session.IdleTimeout}); err != nil {
			return err
		}
		slept = true
		return nil
	})
	if err != nil {
		r.Log.Error("idle check", "session", lv.id, "err", err)
		return
	}
	if slept {
		lv.asleep = true
		go r.stop(lv.dir, time.Second, 2*time.Second)
	}
}

// Sleep puts the newest session of thread to sleep now, as the idle period
// would (checkIdle): only an idle one this runner follows, with no
// background task running, no open request and no person's terminal
// attached, and stops its agent. It reports whether it did; the session
// keeps its id, so its next input resumes it. The supervisor's waker sleeps
// an idle supervisor this way before it gives a wake a conversation of its
// own (#676).
func (r *Runner) Sleep(ctx context.Context, thread control.ThreadID) (bool, error) {
	var id int64
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, int64(thread)).Scan(&id); err != nil {
		return false, fmt.Errorf("runner: the session of thread %d: %w", thread, err)
	}
	r.mu.Lock()
	lv := r.live[id]
	r.mu.Unlock()
	if lv == nil {
		return false, nil
	}
	lv.mu.Lock()
	defer lv.mu.Unlock()
	return r.sleep(ctx, lv)
}

// sleep is Sleep of lv, with lv.mu held.
func (r *Runner) sleep(ctx context.Context, lv *live) (bool, error) {
	id := lv.id
	if lv.asleep || lv.failed || lv.taken != nil || r.attached(lv) {
		return false, nil
	}
	slept := false
	err := r.Store.Update(ctx, func(tx *store.Tx) error {
		slept = false
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id = ?`, id).Scan(&state); err != nil {
			return err
		}
		if control.State(state) != control.Idle {
			return nil
		}
		background, _, err := r.background(ctx, tx, id)
		if err != nil {
			return err
		}
		// Input this run's agent was given and no turn has reported yet, as
		// a message the codec held until the last turn ended. A row an
		// earlier run left, which nothing reports any more, does not count.
		var pending, open int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_inputs WHERE session_id = ? AND stale = 0`, id).Scan(&pending); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE session_id = ? AND state = 'open'`, id).Scan(&open); err != nil {
			return err
		}
		if background > 0 || pending > 0 || open > 0 {
			return nil
		}
		if err := r.Machine.ApplyTx(ctx, tx, id, session.Input{Kind: session.IdleTimeout}); err != nil {
			return err
		}
		slept = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("runner: putting session %d to sleep: %w", id, err)
	}
	if slept {
		lv.drain = time.Now().Add(r.usageDrain())
	}
	return slept, nil
}

// attached reports whether a person's terminal is attached to the
// session's host, or was within the idle period: while it is, the session
// is not put to sleep, and the idle period starts again when it goes. The
// host keeps the count, so it holds across a daemon restart. A host that
// does not answer has none attached. A terminal that attaches after this
// check is safe too: AttachTerminal restarts the idle period in a write
// transaction, which the sleep's transaction below then sees.
func (r *Runner) attached(lv *live) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if lv.remote {
		remote, ok := lv.io.(*machineio.Remote)
		if !ok {
			return true
		}
		m, err := remote.Info(ctx)
		return err != nil || m.Attached > 0 || m.DetachedMS > 0 && r.activeAge(ctx, r.Store.DB, lv.id, time.UnixMilli(m.DetachedMS)) < r.idlePeriod()
	}
	if w, ok := heldOf(lv.dir); ok {
		// A person who looks at the tmux window, or types in it.
		a, err := r.tmuxHolder().Attached(ctx, w)
		return a || err != nil // cannot tell: not put to sleep under them
	}
	c, err := host.DialContext(ctx, lv.dir)
	if err != nil {
		return false
	}
	defer c.Close()
	n, detached, err := c.Attached(ctx)
	if err != nil {
		return false
	}
	return n > 0 || !detached.IsZero() && time.Since(detached) < r.idlePeriod()
}

// shutWait bounds how long Shut waits for a closed session's agent to end.
const shutWait = 15 * time.Second

// Shut closes a session and makes sure its agent has ended. Unless the
// session is closed already, it applies session.Close with reason, while it
// holds the session's lock, so no step reads what the agent reports after
// it, and in a transaction that refuses a session that is no longer its
// thread's newest (one replaced meanwhile; the new session is made in the
// transaction that stores the old one's failure, so one of the two sees the
// other), and an idle one whose last turn left background tasks running and
// was not stopped. Then it stops the
// session's host, when one runs, and waits until the host and the runner's
// following of it have ended. A closed session is only stopped and waited
// for: a Close repeated after a stop that failed tries again, and a removal
// knows that the agent has ended. also runs in the transaction of the
// change, before it, and not for a session that is closed already.
func (r *Runner) Shut(ctx context.Context, id int64, reason string, also ...func(*store.Tx) error) error {
	remote, err := r.isRemote(ctx, id)
	if err != nil {
		return err
	}
	var workerIO *machineio.Remote
	if remote {
		workerIO, err = r.remoteSession(ctx, id)
		if err != nil {
			return err
		}
		if _, err = workerIO.Alive(ctx); err != nil {
			return err
		}
	}
	r.mu.Lock()
	lv := r.live[id]
	r.mu.Unlock()
	apply := func() error {
		var thread control.ThreadID
		err := r.Store.Update(ctx, func(tx *store.Tx) error {
			var (
				state string
				newer int
			)
			if err := tx.QueryRowContext(ctx, `SELECT s.thread_id, s.state,
				(SELECT count(*) FROM sessions n WHERE n.thread_id = s.thread_id AND n.id > s.id)
				FROM sessions s WHERE s.id = ?`, id).Scan(&thread, &state, &newer); err != nil {
				return fmt.Errorf("runner: session %d: %w", id, err)
			}
			if control.State(state) == control.Closed {
				return nil
			}
			if newer > 0 {
				return fmt.Errorf("runner: session %d: its thread has a newer session: %w", id, session.ErrRefused)
			}
			// As the sleep does: a session whose last turn left background
			// tasks running is still working, though it is idle. Stopping
			// its turn is how a person says that this may end too.
			background, stopped, err := r.background(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("runner: session %d: %w", id, err)
			}
			if background > 0 && !stopped && control.State(state) == control.Idle {
				return fmt.Errorf("runner: session %d: %d background tasks still run: %w", id, background, session.ErrRefused)
			}
			// Input or an answer the core accepted and no turn has reported
			// yet, such as a message the codec holds until the turn that
			// was running ended: closing would lose it without a trace.
			// Checked here, so none is accepted between this and the Close.
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_inputs WHERE session_id = ?`, id).Scan(&pending); err != nil {
				return fmt.Errorf("runner: session %d: %w", id, err)
			}
			if pending > 0 && control.State(state) == control.Idle {
				return fmt.Errorf("runner: session %d: %d pending inputs not reported by a turn: %w", id, pending, session.ErrRefused)
			}
			for _, f := range also {
				if err := f(tx); err != nil {
					return err
				}
			}
			return r.Machine.ApplyTx(ctx, tx, id, session.Input{Kind: session.Close, Reason: reason})
		})
		if err == nil && r.Unconfine != nil {
			r.Unconfine(thread, id)
		}
		return err
	}
	if lv != nil {
		lv.mu.Lock()
		err := apply()
		if err == nil {
			lv.asleep = true // nothing more is read: its agent is being stopped
		}
		lv.mu.Unlock()
		if err != nil {
			return err
		}
	} else if err := apply(); err != nil {
		return err
	}
	if remote {
		_, err := workerIO.End(ctx, "end-"+workerIO.Target.Run, time.Second, 2*time.Second)
		return err
	}
	dir := StateDir(r.Root, id)
	running := func() bool { return r.alive(dir).State == holder.Running }
	if t, ok := takenOf(dir); ok {
		// A window taken in: its relay host goes, and the window is left
		// as it is, its hooks reporting again.
		if err := r.letGo(ctx, id, dir, t, nil); err != nil {
			return fmt.Errorf("runner: session %d is closed, but letting its window go: %w", id, err)
		}
	}
	if running() {
		if _, err := r.stop(dir, time.Second, 2*time.Second); err != nil {
			return fmt.Errorf("runner: session %d is closed, but stopping its agent: %w", id, err)
		}
	}
	deadline := time.After(shutWait)
	for running() {
		select {
		case <-deadline:
			return fmt.Errorf("runner: session %d is closed, but its host has not ended", id)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.poll()):
		}
	}
	if lv != nil {
		select {
		case <-lv.done:
		case <-deadline:
			return fmt.Errorf("runner: session %d is closed, but its agent has not ended", id)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// replacement is a session replaceTx inserted, to be launched once its
// transaction is committed.
type replacement struct {
	l     core.Launch
	first firstInput
}

// replaceTx inserts, in tx, a new session in the same workspace for a
// session whose conversation the agent no longer has, with the input the
// old one was woken for. It does nothing and returns nil when the session
// has not failed or its thread already has a newer session. The new session
// is inserted with its first input and launch 1: a daemon that stops before
// the launch launches it when it comes back (core.Core.Recover), and the
// input, whose key was accepted, reaches the agent once.
func (r *Runner) replaceTx(ctx context.Context, tx *store.Tx, lv *live) (*replacement, error) {
	var (
		l         core.Launch
		state, fi string
		tid       int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT s.state, s.thread_id, s.agent, s.model, s.effort, s.first_input
		FROM sessions s WHERE s.id = ?`, lv.id).
		Scan(&state, &tid, &l.Agent, &l.Model, &l.Effort, &fi); err != nil {
		return nil, err
	}
	l.Thread = control.ThreadID(tid)
	var newer int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE thread_id = ? AND id > ?`, tid, lv.id).Scan(&newer); err != nil {
		return nil, err
	}
	if control.State(state) != control.Failed || newer > 0 {
		return nil, nil
	}
	var old firstInput
	if fi != "" {
		json.Unmarshal([]byte(fi), &old)
	}
	// What the new session's agent gets before the input: the thread's
	// earlier conversation, as this transaction sees it.
	earlier, err := r.earlier(ctx, tx, l.Thread)
	if err != nil {
		r.Log.Error("the thread's earlier context", "session", lv.id, "err", err) // the new session starts without it
	}
	// The input the old session was woken for, as a new session's first
	// input: not sent, not written, and in the old session's workspace. One
	// woken with no input, as for an attach, is replaced by one started with
	// none (#528).
	dir := lv.spec.Dir
	if lv.remote {
		if err := tx.QueryRowContext(ctx, `SELECT workspace FROM session_machine_runs WHERE session_id=?`, lv.id).Scan(&dir); err != nil {
			return nil, err
		}
	}
	first := firstInput{AcceptedOperation: old.AcceptedOperation, Principal: old.Principal, From: old.From, Key: old.Key, Text: old.Text, Link: old.Link,
		Before: old.Before, After: old.After, Via: old.Via, Context: earlier, Dir: dir, NoInput: old.NoInput}
	fb, _ := json.Marshal(first)
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO sessions (thread_id, agent, state, model, effort, first_input, launch, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, tid, l.Agent, string(control.Creating), l.Model, l.Effort, string(fb), now, now)
	if err != nil {
		return nil, err
	}
	if l.Session, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ?`,
		string(control.Creating), now, tid); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(map[string]any{"session": lv.id, "new_session": l.Session, "reason": string(agent.CannotResume)})
	if _, err := tx.Append(ctx, control.Event{Thread: l.Thread, Kind: KindSessionReplaced, Data: data}); err != nil {
		return nil, err
	}
	return &replacement{l: l, first: first}, nil
}

// launchReplacement starts a session replaceTx inserted.
func (r *Runner) launchReplacement(rep replacement) {
	ctx := context.Background()
	l, first := rep.l, rep.first
	r.at("after replace")
	l.AcceptedOperation = first.AcceptedOperation
	l.Dir, l.Principal, l.From, l.Key, l.Context = first.Dir, first.Principal, first.From, first.Key, first.Context
	l.Input, l.NoInput = first.Input(), first.NoInput
	if err := r.Start(ctx, l); err != nil {
		if errors.Is(err, core.ErrPlacementWait) || r.Admit != nil {
			_, _ = r.Store.DB.Exec(`UPDATE sessions SET io_error=? WHERE id=?`, "waiting for room on the recorded machine", l.Session)
			return
		}
		r.Log.Error("starting the new session", "session", l.Session, "err", err)
		if err := r.Machine.Apply(ctx, l.Session, session.Input{Kind: session.Exit, Reason: "start: " + err.Error()}); err != nil {
			r.Log.Error("recording a failed start", "session", l.Session, "err", err)
		}
	}
}

// KindSessionReplaced is the event of a thread whose conversation the agent
// no longer has: a new session starts in the same workspace.
const KindSessionReplaced control.EventKind = "session_replaced"

// checkStart fails a session that has not reported its start in time,
// with the end of its terminal log for a person, and stops its processes.
func (r *Runner) checkStart(lv *live) {
	if !r.sessionChecked(lv.id) || lv.catchingUp || lv.ready || r.activeAge(context.Background(), r.Store.DB, lv.id, lv.started) < r.startTimeout() {
		return
	}
	var state string
	if err := r.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, lv.id).Scan(&state); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.Log.Error("state", "session", lv.id, "err", err)
		}
		return
	}
	if control.State(state) != control.Creating {
		lv.ready = true
		return
	}
	var tail []byte
	if _, ok := heldOf(lv.dir); ok {
		tail = heldTail(lv.dir, 4000)
	} else if lv.client != nil {
		tail, _ = r.own().Tail(context.Background(), r.window(lv.dir), 4000)
	}
	if err := r.Machine.Apply(context.Background(), lv.id, session.Input{Kind: session.StartTimeout, Detail: string(tail)}); err != nil {
		r.Log.Error("start timeout", "session", lv.id, "err", err)
		return
	}
	lv.failed = true
	go r.stop(lv.dir, time.Second, time.Second)
}

// Deliver gives input to a session's agent.
func (r *Runner) Deliver(ctx context.Context, id, pending int64, in control.Input) error {
	lv, err := r.get(id)
	if err != nil {
		r.dropPending(pending)
		return err
	}
	r.at("before pending dispatch")
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if lv.ended {
		return nil
	}
	if r.AwayEnabled {
		if err := r.dispatchAllowed(lv); err != nil {
			return err
		}
	}
	if current, err := r.pendingCurrent(ctx, id, pending, ""); err != nil || !current {
		return err
	}
	return r.handOver(lv, pending, false, func() (agent.Actions, error) {
		return lv.codec.Input(agent.Input{ID: strconv.FormatInt(pending, 10), Text: in.Text})
	})
}

// Answer gives the answer of an open request to a session's agent.
func (r *Runner) Answer(ctx context.Context, id, pending int64, req string, ans control.Answer) error {
	lv, err := r.get(id)
	if err != nil {
		r.dropPending(pending)
		return err
	}
	r.at("before pending dispatch")
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if lv.ended {
		return nil
	}
	if r.AwayEnabled {
		if err := r.dispatchAllowed(lv); err != nil {
			return err
		}
	}
	if current, err := r.pendingCurrent(ctx, id, pending, req); err != nil || !current {
		return err
	}
	return r.handOver(lv, pending, true, func() (agent.Actions, error) {
		return lv.codec.Answer(agent.Answer{Request: req, Text: ans.Text, Choices: ans.Choices, Allow: ans.Allow})
	})
}

// pendingCurrent rejects dispatch delayed until after a new agent retired
// the accepted call. The selected live codec is locked by the caller, so
// its follower cannot finish and permit a replacement until dispatch ends.
func (r *Runner) pendingCurrent(ctx context.Context, id, pending int64, request string) (bool, error) {
	var n int
	err := r.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM pending_inputs
		WHERE id = ? AND session_id = ? AND request = ? AND stale = 0 AND written = 0`, pending, id, request).Scan(&n)
	return n != 0, err
}

// recordStop records that by stopped turn of session id: the person on the
// turn, unless it names someone already, and a stop event of the thread,
// in one transaction. It reports whether the turn names by now.
func (r *Runner) recordStop(ctx context.Context, id, turn int64, by session.Stopper) (names bool, err error) {
	err = r.Store.Update(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE turns SET stopped_by = ? WHERE id = ? AND stopped_by = ''`, string(by.Principal), turn)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		names = n > 0
		var thread int64
		if err := tx.QueryRowContext(ctx, `SELECT thread_id FROM sessions WHERE id = ?`, id).Scan(&thread); err != nil {
			return err
		}
		d := map[string]any{"session": id, "turn": turn, "principal": string(by.Principal)}
		if by.From != "" {
			d["from"] = by.From
		}
		if by.Via != "" {
			d["via"] = by.Via
		}
		data, err := json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = tx.Append(ctx, control.Event{Thread: control.ThreadID(thread), Kind: session.KindStop, Data: data})
		return err
	})
	return names, err
}

// Interrupt stops a session's running turn: the agent's own interrupt
// first. When that turn has not ended after the grace period, every process
// of the session is stopped; a turn started after it is not watched. A
// named turn (not 0) that is not the running one is refused, and nothing is
// interrupted. A Stop's (by with a Principal) names its person on the turn,
// unless the turn names someone already, and writes one stop event, both
// in one transaction before the interrupt is sent. An interrupt that the
// agent's window did not take as it is (holder.NotSent), or that the codec
// cannot carry out yet (Actions.NotInterrupted), is none: the turn
// goes on, its Stop's name is taken back, and the error says
// control.ErrNotInterrupted.
func (r *Runner) Interrupt(ctx context.Context, id, named int64, by session.Stopper) error {
	lv, err := r.get(id)
	if err != nil {
		return err
	}
	// The turn the interrupt is for, read under lv.mu, as the runner's steps
	// that end turns are: none ends between this and the interrupt, and so
	// none starts (a Send starts one only when none is open). Once it has
	// ended the watch is over, even when the next turn has started.
	lv.mu.Lock()
	if r.AwayEnabled {
		if err := r.dispatchAllowed(lv); err != nil {
			lv.mu.Unlock()
			return err
		}
	}
	var turn int64
	r.Store.DB.QueryRow(`SELECT coalesce(max(id), 0) FROM turns WHERE session_id = ? AND ended_at IS NULL`, id).Scan(&turn)
	if named != 0 && named != turn {
		lv.mu.Unlock()
		return fmt.Errorf("runner: session %d: turn %d is not the running turn: %w", id, named, session.ErrRefused)
	}
	names := false
	if by.Principal != "" && turn != 0 {
		// Before the interrupt, which can end the turn at once: a reader
		// of its end finds who stopped it, after the stop event. The first
		// stop names it.
		if names, err = r.recordStop(ctx, id, turn, by); err != nil {
			lv.mu.Unlock()
			return fmt.Errorf("runner: session %d: record who stops turn %d: %w", id, turn, err)
		}
	}
	a := lv.codec.Interrupt()
	err = r.do(lv, a)
	var no *holder.NotSent
	if a.NotInterrupted || errors.As(err, &no) && no.Refused || errors.Is(err, core.ErrMachineUnchecked) {
		if names {
			// Its end, whatever it is, is not this Stop's.
			if _, err := r.Store.DB.Exec(`UPDATE turns SET stopped_by = '' WHERE id = ?`, turn); err != nil {
				r.Log.Error("interrupt: forget who stopped the turn", "session", id, "err", err)
			}
		}
		lv.mu.Unlock()
		if errors.Is(err, core.ErrMachineUnchecked) {
			return err
		}
		if err != nil {
			return fmt.Errorf("runner: session %d: %w: %w", id, control.ErrNotInterrupted, err)
		}
		if a.InterruptReason != "" {
			return fmt.Errorf("runner: session %d: %s: %w", id, a.InterruptReason, control.ErrNotInterrupted)
		}
		return fmt.Errorf("runner: session %d: %w", id, control.ErrNotInterrupted)
	}
	lv.mu.Unlock()
	if turn == 0 {
		return nil // no turn runs: nothing to stop later
	}
	go func() {
		began := time.Now()
		for !r.sessionChecked(id) || r.activeAge(context.Background(), r.Store.DB, id, began) < r.interruptGrace() {
			select {
			case <-lv.stop:
				return
			default:
			}
			r.at("interrupt watch")
			var state string
			if r.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state) == nil &&
				control.State(state) != control.Running {
				return
			}
			var ended sql.NullInt64
			if r.Store.DB.QueryRow(`SELECT ended_at FROM turns WHERE id = ?`, turn).Scan(&ended) == nil && ended.Valid {
				return
			}
			time.Sleep(r.poll())
		}
		r.at("interrupt watch: stop")
		// Once more, under the lock the runner's steps hold when they end a
		// turn, and held until the session is stopped: a turn that ended
		// in the last poll, and the turn after it, are left running (#494).
		// A host that does not answer the stop holds the lock no longer
		// than stopHold; the stop goes on without it.
		lv.mu.Lock()
		defer lv.mu.Unlock()
		if !r.sessionChecked(id) {
			return
		}
		var state string
		if r.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state) == nil &&
			control.State(state) != control.Running {
			return
		}
		var ended sql.NullInt64
		if r.Store.DB.QueryRow(`SELECT ended_at FROM turns WHERE id = ?`, turn).Scan(&ended) == nil && ended.Valid {
			return
		}
		if lv.taken != nil {
			// A window a person started: nothing hataori does ends its
			// process. The turn ends when the agent reports it.
			r.Log.Warn("the turn did not end after the interrupt; the window taken in is left running", "session", id)
			return
		}
		r.Log.Warn("the turn did not end after the interrupt; stopping the session", "session", id)
		r.at("interrupt watch: stopping")
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			r.stop(lv.dir, time.Second, 2*time.Second)
		}()
		select {
		case <-stopped:
		case <-time.After(stopHold):
			r.Log.Warn("the session's host has not answered the stop; the runner goes on", "session", id)
		}
	}()
	return nil
}

// stopHold is how long the interrupt's watchdog holds the session's lock
// while its host stops the session: its SIGINT and SIGTERM periods, and
// time for the host to answer.
const stopHold = 10 * time.Second

// claim reports whether session id was not started in this process before,
// and marks it started.
func (r *Runner) claim(id int64) bool {
	r.startedMu.Lock()
	defer r.startedMu.Unlock()
	if r.started == nil {
		r.started = map[int64]bool{}
	}
	if r.started[id] {
		return false
	}
	r.started[id] = true
	return true
}
