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
	"log/slog"
	"os"
	"path/filepath"
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
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Runner runs the sessions of one instance. It implements core.Runner.
type Runner struct {
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

	Instructions   string        // appended to every agent's own
	StartTimeout   time.Duration // for the agent to report its start; 60s when zero
	InterruptGrace time.Duration // after an interrupt, before the session's processes are stopped; 5s when zero
	Poll           time.Duration // how often the logs are read; 50ms when zero
	IdlePeriod     time.Duration // idle this long, a session's agent is stopped; 30 minutes when zero
	ContextBytes   int           // the cap on the earlier context a replacing session starts with; 8192 when zero
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
	// TrustFiles gives each launch the files of its scope that a start-up
	// dialog may name and the session may be let past it for
	// (agent.Spec.TrustFiles). It is called at every launch; nil gives none.
	TrustFiles func(scope string) []string
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

	point func(name string) // for tests: called at the named points of a launch
}

var _ core.Runner = (*Runner)(nil)

// live is a session the runner follows.
type live struct {
	id      int64
	dir     string
	adapter agent.Adapter
	spec    agent.Spec
	codec   agent.Codec
	client  *host.Client // to the session host: the relay of hook calls, and the first sign that it answers
	started time.Time
	ready   bool   // the session left creating; no start check any more
	resend  bool   // after a reconnect: send a first input that was not confirmed
	pending bool   // after a reconnect: hand over pending input once the logs are read
	held    bool   // lv.mu is held from follow until the loop's first step has handed over the pending input
	upTo    int64  // the last pending input there was at the reconnect; later ones are handed over by their call
	failed  bool   // the start timed out; the session is being stopped
	asleep  bool   // put to sleep: its agent is being stopped, and nothing more is read
	screen  bool   // the codec watches the terminal; until it says StopScreen
	seenTo  uint64 // the position in the host's run up to which the codec was given its terminal
	// taken is the window a person started that the session was taken in
	// from; nil for a session hataori's own host holds. Its input goes to
	// that window, and nothing ends the window's process.
	taken *takenIn

	mu     sync.Mutex // the codec, the offsets and the actions, one at a time
	seq    uint64
	offset int64            // in events.ndjson
	start  int64            // where the current agent's lines begin in events.ndjson
	files  map[string]int64 // followed files and their offsets
	stop   chan struct{}
	done   chan struct{}
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
}

// ConfinedLaunch is one launch of a session whose thread was started
// confined, as Runner.Confine is told it.
type ConfinedLaunch struct {
	Session  int64
	Thread   control.ThreadID
	Scope    string
	StateDir string
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
func (r *Runner) Start(ctx context.Context, l core.Launch) error {
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
	first := firstInput{Principal: l.Principal, From: l.From, Key: l.Key, Text: l.Input.Text, Context: l.Context, Link: l.Input.Link,
		Before: l.Input.Before, After: l.Input.After}
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
	// The scope's config as it is at this launch, not as it was at the
	// session's first.
	spec.TrustFiles = nil
	if r.TrustFiles != nil {
		spec.TrustFiles = r.TrustFiles(scope)
	}
	// What the adapter gets: the spec, with what this launch alone is given.
	// The stored spec keeps the confinement, not the MCP servers, whose
	// credentials hold for this launch only.
	run := spec
	if spec.Confine != nil {
		spec.Confine = &agent.Confinement{}
		if r.Confine == nil {
			return agent.Spec{}, fmt.Errorf("runner: session %d is confined, and nothing gives it its MCP servers; it was not started", id)
		}
		c, err := r.Confine(ctx, ConfinedLaunch{Session: id, Thread: thread, Scope: scope, StateDir: dir})
		if err != nil {
			return agent.Spec{}, fmt.Errorf("runner: session %d: confinement: %w", id, err)
		}
		run.Confine = &agent.Confinement{MCP: c.MCP}
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
	sb, _ := json.Marshal(storedSpec{Agent: agentName, Spec: spec})
	fb, _ := json.Marshal(first)
	var before, recorded int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT launch, host_pid FROM sessions WHERE id = ?`, id).Scan(&before, &recorded); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	// launch = 2: the host is being started. Until it is recorded as
	// started, a daemon that comes back launches the session again, and
	// keeps the host it finds running instead of starting another.
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET spec = ?, first_input = ?, launch = 2 WHERE id = ?`,
		string(sb), string(fb), id); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
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
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET host_pid = ?, launch = 0 WHERE id = ?`, w.Process, id); err != nil {
		return agent.Spec{}, fmt.Errorf("runner: %w", err)
	}
	return spec, nil
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
// conversation, and sends in once the agent reports that it is ready.
func (r *Runner) Resume(ctx context.Context, id int64, principal control.Principal, from, key string, in control.Input) error {
	return r.resume(ctx, id, firstInput{Principal: principal, From: from, Key: key, Text: in.Text, Wake: true, Link: in.Link,
		Before: in.Before, After: in.After})
}

// Wake starts the agent of an asleep session again, resuming its
// conversation, and gives it no input: the session becomes idle.
func (r *Runner) Wake(ctx context.Context, id int64, principal control.Principal) error {
	return r.resume(ctx, id, firstInput{Principal: principal, Wake: true, NoInput: true})
}

func (r *Runner) resume(ctx context.Context, id int64, first firstInput) error {
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
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT spec, agent_session_id, launch FROM sessions WHERE id = ?`, id).
		Scan(&specJSON, &resumeID, &launching); err != nil {
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
	if resumeID == "" {
		return fmt.Errorf("runner: session %d never reported its id; it cannot be resumed", id)
	}
	// launch = 2: a daemon that died had begun this launch, after the old
	// host was gone; a host running now is the new one.
	if launching != 2 {
		if !r.alive(s.Spec.StateDir).Ended() {
			return fmt.Errorf("runner: session %d: its host is still running", id)
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
	if t, ok := takenOf(s.Spec.StateDir); ok {
		// Left by a take-in that ended another way: the new agent's input
		// must not go to the window.
		if err := r.letGo(ctx, id, s.Spec.StateDir, t, nil); err != nil {
			return fmt.Errorf("runner: session %d: %w", id, err)
		}
	}
	spec := s.Spec
	spec.ResumeID = resumeID
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
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET log_offset = ?, log_start = ? WHERE id = ?`, n, n, id); err != nil {
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
// sleep, has its agent stopped.
func (r *Runner) Reconnect(ctx context.Context) error {
	if err := r.stopSleepers(ctx); err != nil {
		return err
	}
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
		go r.stop(dir, time.Second, 2*time.Second)
	}
	return nil
}

// follow starts following a session. On reconnect, the codec first reads
// what was read before, with its results discarded, to rebuild its state.
// A codec that watches the screen is given its terminal from the first
// byte of the host's current run: what the agent it follows wrote, and
// nothing an earlier run left in the log (#505, #520).
func (r *Runner) follow(id int64, ad agent.Adapter, spec agent.Spec, m mode) error {
	resumed := m == reconnect
	lv := &live{id: id, dir: spec.StateDir, adapter: ad, codec: ad.NewCodec(spec), spec: spec,
		files: map[string]int64{}, stop: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
	if t, ok := takenOf(spec.StateDir); ok {
		lv.taken = &t
	}
	var created int64
	err := r.Store.DB.QueryRow(`SELECT last_seq, log_offset, log_start, created_at FROM sessions WHERE id = ?`, id).
		Scan(&lv.seq, &lv.offset, &lv.start, &created)
	if err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
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
	if resumed {
		replies = r.warm(lv)
		// Before any new line is read: input written before the restart,
		// so that the report of it, if it comes, is known for what it is.
		r.handOverWritten(lv)
	}
	if c, err := host.Dial(lv.dir); err == nil {
		lv.client = c
	}
	if resumed && lv.client != nil {
		// Replies decided before the daemon died and not sent: send those
		// whose call still waits. The host refuses the others anyway.
		waiting, _ := lv.client.Waiting()
		open := map[string]bool{}
		for _, w := range waiting {
			open[w.ID] = true
		}
		var again agent.Actions
		for _, rep := range replies {
			if open[rep.Call] {
				again.Reply = append(again.Reply, rep)
			}
		}
		r.act(lv, again)
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
	rows, err := r.Store.DB.Query(`SELECT id, request, data FROM pending_inputs
		WHERE session_id = ? AND id <= ? AND written = 0 ORDER BY id`, lv.id, lv.upTo)
	if err != nil {
		r.Log.Error("pending input", "session", lv.id, "err", err)
		return
	}
	type row struct {
		id        int64
		req, data string
	}
	var found []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.req, &x.data); err != nil {
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
					return lv.codec.Input(agent.Input{ID: strconv.FormatInt(x.id, 10), Text: in.Text})
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
		r.do(lv, r.inputActions(lv, agent.Input{ID: firstInputID, Text: f.Context + f.Input().ForAgent(), Written: true}))
		lv.resend = false
	}
	rows, err := r.Store.DB.Query(`SELECT id, data FROM pending_inputs
		WHERE session_id = ? AND request = '' AND written = 1 ORDER BY id`, lv.id)
	if err != nil {
		r.Log.Error("written input", "session", lv.id, "err", err)
		return
	}
	var ins []agent.Input
	for rows.Next() {
		var id int64
		var data string
		var in control.Input
		if rows.Scan(&id, &data) == nil && json.Unmarshal([]byte(data), &in) == nil {
			ins = append(ins, agent.Input{ID: strconv.FormatInt(id, 10), Text: in.Text, Written: true})
		}
	}
	rows.Close()
	for _, in := range ins {
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
// again.
func (r *Runner) markWritten(lv *live, ids []string) error {
	err := r.Store.Update(context.Background(), func(tx *store.Tx) error {
		ctx := context.Background()
		for _, id := range ids {
			if id == firstInputID {
				var fi string
				if err := tx.QueryRowContext(ctx, `SELECT first_input FROM sessions WHERE id = ?`, lv.id).Scan(&fi); err != nil {
					return err
				}
				var f firstInput
				if fi == "" || json.Unmarshal([]byte(fi), &f) != nil {
					continue
				}
				f.Written = true
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
			if _, err := tx.ExecContext(ctx, `UPDATE pending_inputs SET written = 1 WHERE id = ? AND session_id = ?`, row, lv.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.Log.Error("marking input written", "session", lv.id, "err", err)
	}
	return err
}

// handOver carries out what the codec makes of one pending input or
// answer. Input stays pending until the agent reports the turn it starts
// (see commit): a codec may hold it until the agent can take it. An answer
// is removed once the host has taken it. One the codec refuses is removed,
// and its error returned. lv.mu is held.
func (r *Runner) handOver(lv *live, pending int64, answer bool, f func() (agent.Actions, error)) error {
	a, err := f()
	if err == nil {
		if err = r.do(lv, a); err != nil || !answer {
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
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET log_offset = ? WHERE id = ?`, b.offset, session); err != nil {
		return err
	}
	for p, o := range b.files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_files (session_id, path, offset) VALUES (?, ?, ?)
			ON CONFLICT (session_id, path) DO UPDATE SET offset = excluded.offset`, session, p, o); err != nil {
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
	}
	return nil
}

// warm feeds the codec what was read before the stored offsets, in the
// order the steps read it (session_reads). Lines that no stored range
// covers, read before ranges were stored, come first: the host's log, then
// each followed file, each from where the current agent's lines begin in
// it, as they were replayed then. What is before that is an earlier
// agent's, or was skipped while the session was put to sleep, and is not
// given.
// It returns the replies to hook calls it produced: one that was never sent
// because the daemon died first still has its call waiting at the host.
func (r *Runner) warm(lv *live) []agent.HookReply {
	starts, reads, err := r.storedReads(lv)
	if err != nil {
		// Without the ranges the order is not known; the lines are still
		// given, from where the current agent's begin if that is known, in
		// the order used before ranges were stored.
		r.Log.Error("warm: stored reads", "session", lv.id, "err", err)
		reads = nil
	}
	logPath := filepath.Join(lv.dir, host.EventsFile)
	data := map[string][]byte{}
	content := func(p string) []byte {
		if b, ok := data[p]; ok {
			return b
		}
		b, _ := os.ReadFile(p)
		data[p] = b
		return b
	}
	// Where each source's stored ranges begin; what is before is replayed
	// as it was before ranges were stored.
	first := map[string]int64{}
	for _, rd := range reads {
		if _, ok := first[rd.path]; !ok {
			first[rd.path] = rd.start
		}
	}
	upTo := func(p string, end int64) int64 {
		if f, ok := first[p]; ok && f < end {
			return f
		}
		return end
	}
	var replies []agent.HookReply
	give := func(src func([]byte) agent.Source, b []byte, from, to int64) {
		for _, l := range lines(b, from, to) {
			replies = append(replies, lv.codec.Decode(src(l), l).Reply...)
		}
	}
	file := func([]byte) agent.Source { return agent.File }
	give(source, content(logPath), lv.start, upTo("", lv.offset))
	for p, off := range lv.files {
		give(file, content(p), starts[p], upTo(p, off))
	}
	for _, rd := range reads {
		if rd.path == "" {
			give(source, content(logPath), rd.start, rd.end)
		} else {
			give(file, content(rd.path), rd.start, rd.end)
		}
	}
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
	rows, err = r.Store.DB.Query(`SELECT path, start, end FROM session_reads WHERE session_id = ? ORDER BY n`, lv.id)
	if err != nil {
		return starts, nil, err // the starts still hold without the ranges
	}
	defer rows.Close()
	var out []readRange
	for rows.Next() {
		var rd readRange
		if err := rows.Scan(&rd.path, &rd.start, &rd.end); err != nil {
			return starts, nil, err
		}
		limit, ok := lv.offset, rd.path == ""
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
	r.live = nil
	r.mu.Unlock()
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
		if time.Since(lastTick) >= time.Second {
			lastTick = time.Now()
			lv.mu.Lock()
			r.do(lv, lv.codec.Tick(time.Now()))
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
	events  []agent.Event
	actions []agent.Actions
	offset  int64
	files   map[string]int64
	reads   []readRange // what was given to the codec, in order
}

// readRange is complete lines of the host's log (path "") or of a followed
// file, from start to end, given to the codec at once. The ranges a step
// read are stored with its events, so that a codec rebuilt later is given
// the lines in the same order (warm).
type readRange struct {
	path       string
	start, end int64
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
func (r *Runner) stepLocked(lv *live) bool {
	if lv.asleep {
		return !r.polled(lv.dir).Ended()
	}
	exited := r.polled(lv.dir).Ended() // looked at before reading, so nothing written before the exit is missed
	b := r.read(lv)
	r.watchScreen(lv, &b)
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
		// held to write later.
		r.handOverWritten(lv)
		lv.pending = true
		r.Store.DB.QueryRow(`SELECT coalesce(max(id), 0) FROM pending_inputs WHERE session_id = ?`, lv.id).Scan(&lv.upTo)
		return true
	}
	lv.offset = b.offset
	for p, o := range b.files {
		lv.files[p] = o
	}
	for _, e := range b.events {
		if e.Kind == agent.SessionEnded && lv.taken != nil && e.Reason != agent.WrongSession && e.Reason != clearReason {
			// The agent in the window ended: the window closed.
			go r.windowClosed(lv)
			continue
		}
		if e.Kind == agent.SessionEnded && e.Reason == agent.CannotResume {
			go r.replace(lv)
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
	for _, a := range b.actions {
		r.act(lv, a)
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
	if lv.failed && lv.client == nil {
		return false // never had a host: nothing more will come
	}
	r.checkStart(lv)
	return true
}

// read decodes what is new in the files the codec follows, then in the
// host's log of hook calls and output, then in the files those calls named
// and anything new in the others. An agent writes a line of its transcript
// before it reports what follows it, such as a turn's last message before
// the turn's end, so a line written before a hook call is decoded first
// even when both are new in one step.
func (r *Runner) read(lv *live) batch {
	b := batch{offset: lv.offset, files: map[string]int64{}}
	for p, off := range lv.files {
		b.files[p] = off
	}
	readFiles := func() {
		for i := 0; i < len(b.files)+1; i++ { // a followed file may name another
			for p, off := range b.files {
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				end := complete(data, off)
				if end > off {
					b.reads = append(b.reads, readRange{path: p, start: off, end: end})
				}
				for _, l := range lines(data, off, end) {
					r.decode(lv, &b, agent.File, l)
				}
				b.files[p] = end
			}
		}
	}
	readFiles()
	if data, err := os.ReadFile(filepath.Join(lv.dir, host.EventsFile)); err == nil {
		end := complete(data, lv.offset)
		if end > lv.offset {
			b.reads = append(b.reads, readRange{start: lv.offset, end: end})
		}
		for _, l := range lines(data, lv.offset, end) {
			r.decode(lv, &b, source(l), l)
		}
		b.offset = end
	}
	readFiles()
	return b
}

// watchScreen gives the codec what the agent wrote to its terminal since
// the last step, while it watches the screen. Once the codec has said
// StopScreen, the terminal is not read again.
func (r *Runner) watchScreen(lv *live, b *batch) {
	if !lv.screen {
		return
	}
	since := func(from uint64) ([]byte, uint64) { return r.own().Since(r.window(lv.dir), from) }
	if _, ok := heldOf(lv.dir); ok {
		since = func(from uint64) ([]byte, uint64) { return heldScreen(lv.dir, from) }
	}
	data, next := since(lv.seenTo)
	lv.seenTo = next
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
	var first *firstInput
	seq := lv.seq
	err := r.Store.Update(context.Background(), func(tx *store.Tx) error {
		ctx := context.Background()
		seq = lv.seq
		first = nil
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
		if err := saveRead(ctx, tx, lv.id, b); err != nil {
			return err
		}
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
		switch {
		case f.NoInput && control.State(state) == control.Idle:
			// Woken with nothing to say: ready is all it waited for.
			f = firstInput{}
		case f.NoInput:
			return nil
		case !f.Sent && control.State(state) == control.Idle:
			if err := r.Machine.ApplyTx(ctx, tx, lv.id, session.Input{Kind: session.Send, Principal: f.Principal, From: f.From, Key: f.Key, Text: f.Text, Link: f.Link,
				Before: f.Before, After: f.After}); err != nil {
				return err
			}
			f.Sent = true
			first = &f
		case f.Sent && reported(b.events, firstInputID):
			// The agent reported the turn: delivered.
			f = firstInput{}
		case f.Sent && notDelivered(b.events, firstInputID):
			// The agent never reported it; its turn failed, as the events say.
			f = firstInput{}
		case f.Sent && lv.resend:
			// The daemon died between starting the turn and a report of it:
			// hand it to the codec again.
			first = &f
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
	})
	if err == nil {
		lv.seq = seq
		lv.resend = false
	}
	return first, err
}

// clearReported removes the pending input whose turn the agent reported, or
// whose turn failed because it never reported it:
// the codec names it by the Input.ID it was given, the row's id.
func clearReported(ctx context.Context, tx *store.Tx, id int64, evs []agent.Event) error {
	for _, e := range evs {
		gone := e.Kind == agent.TurnStarted || e.Kind == agent.InputTaken || e.Kind == agent.TurnEnded && e.Reason == agent.NotDelivered
		if !gone || e.Input == "" {
			continue
		}
		row, err := strconv.ParseInt(e.Input, 10, 64)
		if err != nil {
			continue // not one of ours
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

// notDelivered reports whether a turn among evs failed because the input
// id was never reported.
func notDelivered(evs []agent.Event, id string) bool {
	for _, e := range evs {
		if e.Kind == agent.TurnEnded && e.Reason == agent.NotDelivered && e.Input == id {
			return true
		}
	}
	return false
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
		b := batch{offset: lv.offset, files: map[string]int64{}}
		for p, o := range lv.files {
			b.files[p] = o
		}
		for _, e := range a.Events {
			if e.Kind != agent.Unknown {
				b.events = append(b.events, e)
			}
		}
		if _, err := r.commit(lv, b); err != nil {
			r.Log.Error("commit", "session", lv.id, "err", err)
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
	if len(a.Inputs) > 0 {
		if err := r.markWritten(lv, a.Inputs); err != nil {
			// Not written: the codec, which counts it written, fails its
			// turn as "not delivered" when no report comes.
			a.Keys, a.Write = nil, nil
			if len(a.Reply) == 0 {
				return err
			}
		}
		r.at("input marked")
	}
	defer r.at("input written")
	if lv.client == nil {
		c, err := host.Dial(lv.dir)
		if err != nil {
			r.Log.Error("host unreachable", "session", lv.id, "err", err)
			return err
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
			in = func() error {
				// Only to the agent that was taken in: suspended or ended, the
				// window's foreground is someone's shell, which would run the
				// text as a command.
				if err := r.tmuxHolder().CheckTakeIn(ctx, lv.taken.Window); err != nil {
					return fmt.Errorf("runner: the agent taken in is not the window's foreground process; nothing was sent: %w", err)
				}
				return r.tmuxHolder().Input(ctx, lv.taken.Window, a.Keys)
			}
		}
		if err := in(); err != nil {
			r.Log.Error("input", "session", lv.id, "err", err)
			first = err
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
	if err := r.Machine.Apply(context.Background(), lv.id, in); err != nil && !errors.Is(err, session.ErrRefused) {
		r.Log.Error("exit", "session", lv.id, "err", err)
	}
}

// checkIdle puts the session to sleep when it has been idle for the idle
// period, with no background task running and no open request, and stops
// its agent. The session keeps its id, so its next input resumes it.
func (r *Runner) checkIdle(lv *live) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if lv.asleep || lv.failed || lv.taken != nil { // a window taken in runs as its person keeps it
		return
	}
	ctx := context.Background()
	var state string
	var updated int64
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT state, updated_at FROM sessions WHERE id = ?`, lv.id).Scan(&state, &updated); err != nil ||
		control.State(state) != control.Idle || time.Since(time.UnixMilli(updated)) < r.idlePeriod() {
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
		if control.State(state) != control.Idle || time.Since(time.UnixMilli(updated)) < r.idlePeriod() {
			return nil
		}
		var background, open int
		err := tx.QueryRowContext(ctx, `SELECT background FROM turns WHERE session_id = ? ORDER BY id DESC LIMIT 1`, lv.id).Scan(&background)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
// thread's newest (one replaced meanwhile; replace makes the new session in
// a transaction too, so one of the two sees the other), and an idle one
// whose last turn left background tasks running. Then it stops the
// session's host, when one runs, and waits until the host and the runner's
// following of it have ended. A closed session is only stopped and waited
// for: a Close repeated after a stop that failed tries again, and a removal
// knows that the agent has ended.
func (r *Runner) Shut(ctx context.Context, id int64, reason string) error {
	r.mu.Lock()
	lv := r.live[id]
	r.mu.Unlock()
	apply := func() error {
		return r.Store.Update(ctx, func(tx *store.Tx) error {
			var (
				state string
				newer int
			)
			if err := tx.QueryRowContext(ctx, `SELECT s.state,
				(SELECT count(*) FROM sessions n WHERE n.thread_id = s.thread_id AND n.id > s.id)
				FROM sessions s WHERE s.id = ?`, id).Scan(&state, &newer); err != nil {
				return fmt.Errorf("runner: session %d: %w", id, err)
			}
			if control.State(state) == control.Closed {
				return nil
			}
			if newer > 0 {
				return fmt.Errorf("runner: session %d: its thread has a newer session: %w", id, session.ErrRefused)
			}
			// As the sleep does: a session whose last turn left background
			// tasks running is still working, though it is idle.
			var background int
			err := tx.QueryRowContext(ctx, `SELECT background FROM turns WHERE session_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&background)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("runner: session %d: %w", id, err)
			}
			if background > 0 && control.State(state) == control.Idle {
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
			return r.Machine.ApplyTx(ctx, tx, id, session.Input{Kind: session.Close, Reason: reason})
		})
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

// replace starts a new session in the same workspace for a session whose
// conversation the agent no longer has, with the input the old one was
// woken for. It does nothing when the thread already has a newer session.
// The new session is inserted with its first input and launch 1, in one
// transaction: a daemon that stops before the launch launches it when it
// comes back (core.Core.Recover), and the input, whose key was accepted,
// reaches the agent once.
func (r *Runner) replace(lv *live) {
	ctx := context.Background()
	var (
		l     core.Launch
		first firstInput
	)
	err := r.Store.Update(ctx, func(tx *store.Tx) error {
		l = core.Launch{}
		var state, fi string
		var tid int64
		if err := tx.QueryRowContext(ctx, `SELECT s.state, s.thread_id, s.agent, s.model, s.effort, s.first_input
			FROM sessions s WHERE s.id = ?`, lv.id).
			Scan(&state, &tid, &l.Agent, &l.Model, &l.Effort, &fi); err != nil {
			return err
		}
		l.Thread = control.ThreadID(tid)
		var newer int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE thread_id = ? AND id > ?`, tid, lv.id).Scan(&newer); err != nil {
			return err
		}
		if control.State(state) != control.Failed || newer > 0 {
			return nil
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
		// input: not sent, not written, and in the old session's workspace.
		first = firstInput{Principal: old.Principal, From: old.From, Key: old.Key, Text: old.Text, Link: old.Link,
			Before: old.Before, After: old.After, Context: earlier, Dir: lv.spec.Dir}
		fb, _ := json.Marshal(first)
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `INSERT INTO sessions (thread_id, agent, state, model, effort, first_input, launch, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, tid, l.Agent, string(control.Creating), l.Model, l.Effort, string(fb), now, now)
		if err != nil {
			return err
		}
		if l.Session, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ?`,
			string(control.Creating), now, tid); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"session": lv.id, "new_session": l.Session, "reason": string(agent.CannotResume)})
		_, err = tx.Append(ctx, control.Event{Thread: l.Thread, Kind: KindSessionReplaced, Data: data})
		return err
	})
	if err != nil {
		r.Log.Error("replacing a session that cannot resume", "session", lv.id, "err", err)
		return
	}
	if l.Session == 0 {
		return
	}
	r.at("after replace")
	l.Dir, l.Principal, l.From, l.Key, l.Context = first.Dir, first.Principal, first.From, first.Key, first.Context
	l.Input = first.Input()
	if err := r.Start(ctx, l); err != nil {
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
	if lv.ready || time.Since(lv.started) < r.startTimeout() {
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
	lv.mu.Lock()
	defer lv.mu.Unlock()
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
	lv.mu.Lock()
	defer lv.mu.Unlock()
	return r.handOver(lv, pending, true, func() (agent.Actions, error) {
		return lv.codec.Answer(agent.Answer{Request: req, Text: ans.Text, Choices: ans.Choices, Allow: ans.Allow})
	})
}

// Interrupt stops a session's running turn: the agent's own interrupt
// first. When that turn has not ended after the grace period, every process
// of the session is stopped; a turn started after it is not watched. A
// named turn (not 0) that is not the running one is refused, and nothing is
// interrupted.
func (r *Runner) Interrupt(ctx context.Context, id, named int64) error {
	lv, err := r.get(id)
	if err != nil {
		return err
	}
	// The turn the interrupt is for, read under lv.mu, as the runner's steps
	// that end turns are: none ends between this and the interrupt, and so
	// none starts (a Send starts one only when none is open). Once it has
	// ended the watch is over, even when the next turn has started.
	lv.mu.Lock()
	var turn int64
	r.Store.DB.QueryRow(`SELECT coalesce(max(id), 0) FROM turns WHERE session_id = ? AND ended_at IS NULL`, id).Scan(&turn)
	if named != 0 && named != turn {
		lv.mu.Unlock()
		return fmt.Errorf("runner: session %d: turn %d is not the running turn: %w", id, named, session.ErrRefused)
	}
	r.do(lv, lv.codec.Interrupt())
	lv.mu.Unlock()
	if turn == 0 {
		return nil // no turn runs: nothing to stop later
	}
	go func() {
		deadline := time.Now().Add(r.interruptGrace())
		for time.Now().Before(deadline) {
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
