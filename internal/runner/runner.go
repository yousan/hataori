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
// the session reports SessionReady; otherwise the end of a terminal log is
// asked of the host only to be shown to a person.
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
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Runner runs the sessions of one instance. It implements core.Runner.
type Runner struct {
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

	mu   sync.Mutex
	live map[int64]*live

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
	client  *host.Client
	started time.Time
	ready   bool  // the session left creating; no start check any more
	resend  bool  // after a reconnect: send a first input that was not confirmed
	pending bool  // after a reconnect: hand over pending input once the logs are read
	held    bool  // lv.mu is held from follow until the loop's first step has handed over the pending input
	upTo    int64 // the last pending input there was at the reconnect; later ones are handed over by their call
	failed  bool  // the start timed out; the session is being stopped
	asleep  bool  // put to sleep: its agent is being stopped, and nothing more is read
	screen  bool  // the codec watches the terminal; until the session reports SessionReady
	seenTo  int64 // how much of the terminal the codec was given

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

// Start starts the agent of a new session through a session host. The
// first input is sent once the agent reports that it is ready.
func (r *Runner) Start(ctx context.Context, l core.Launch) error {
	ad, ok := r.Adapters[l.Agent]
	if !ok {
		return fmt.Errorf("runner: no agent %q", l.Agent)
	}
	spec := agent.Spec{Dir: l.Dir, StateDir: StateDir(r.Root, l.Session), Model: l.Model, Effort: l.Effort,
		Instructions: r.Instructions, Approvals: agent.AllowAll}
	first := firstInput{Principal: l.Principal, Key: l.Key, Text: l.Input.Text, Context: l.Context}
	if err := r.launch(ctx, l.Session, l.Agent, ad, spec, first); err != nil {
		return err
	}
	return r.follow(l.Session, ad, spec, fresh)
}

// launch writes what the agent asks for into the session's state directory,
// stores the spec and the first input, and starts the session host.
func (r *Runner) launch(ctx context.Context, id int64, agentName string, ad agent.Adapter, spec agent.Spec, first firstInput) error {
	dir := spec.StateDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	launch, err := ad.Launch(spec)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	for name, b := range launch.Files {
		if !filepath.IsLocal(name) {
			return fmt.Errorf("runner: the agent asks for a file outside its state directory: %q", name)
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("runner: %w", err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return fmt.Errorf("runner: %w", err)
		}
	}
	first.Dir = spec.Dir // so that a launch begun here can be begun again
	sb, _ := json.Marshal(storedSpec{Agent: agentName, Spec: spec})
	fb, _ := json.Marshal(first)
	var before, recorded int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT launch, host_pid FROM sessions WHERE id = ?`, id).Scan(&before, &recorded); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	// launch = 2: the host is being started. Until it is recorded as
	// started, a daemon that comes back launches the session again, and
	// keeps the host it finds running instead of starting another.
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET spec = ?, first_input = ?, launch = 2 WHERE id = ?`,
		string(sb), string(fb), id); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	r.at("before host start")
	pid := hostPID(dir)
	if before == 2 && (pid > 0 && pid != recorded || host.Running(dir)) {
		// A daemon that died had started this host, which runs or has run:
		// it is followed, not started again. The old host of a woken
		// session had let go of the state directory before launch was 2.
		if err := host.WaitReady(dir); err != nil {
			return err
		}
	} else {
		if host.Running(dir) {
			if before != 2 {
				return fmt.Errorf("runner: session %d: another host still holds its state directory", id)
			}
			// The host the daemon that died started took the lock just now.
			if err := host.WaitReady(dir); err != nil {
				return err
			}
		} else if _, err := host.Start(r.Host, dir, host.Spec{Argv: launch.Argv, Env: launch.Env, Terminal: launch.Terminal, Dir: spec.Dir}); err != nil {
			return err
		}
	}
	// The host writes its pid before it listens; a process that found the
	// state directory taken exits, so the file names the one that runs.
	pid = hostPID(dir)
	r.at("after host start")
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET host_pid = ?, launch = 0 WHERE id = ?`, pid, id); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	return nil
}

// hostPID is the pid the host of dir recorded, or 0.
func hostPID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, host.HostPIDFile))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(string(bytes.TrimSpace(b)))
	return pid
}

// at calls the test hook for a point of a launch.
func (r *Runner) at(point string) {
	if r.point != nil {
		r.point(point)
	}
}

// Resume starts the agent of an asleep session again, resuming its
// conversation, and sends in once the agent reports that it is ready.
func (r *Runner) Resume(ctx context.Context, id int64, principal control.Principal, key string, in control.Input) error {
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
		if !host.Gone(s.Spec.StateDir) && !hostLost(s.Spec.StateDir) {
			return fmt.Errorf("runner: session %d: its host is still running", id)
		}
		// An ended host records its end before it lets go of the state
		// directory.
		for deadline := time.Now().Add(5 * time.Second); host.Running(s.Spec.StateDir); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				return fmt.Errorf("runner: session %d: its ended host still holds the state directory", id)
			}
		}
		if err := r.skipRest(ctx, id, s.Spec.StateDir); err != nil {
			return err
		}
		// The status of the ended host would make the new one look ended.
		if err := os.Remove(filepath.Join(s.Spec.StateDir, host.StatusFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("runner: %w", err)
		}
	}
	spec := s.Spec
	spec.ResumeID = resumeID
	if err := r.launch(ctx, id, s.Agent, ad, spec, firstInput{Principal: principal, Key: key, Text: in.Text, Wake: true}); err != nil {
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
			if _, err := tx.ExecContext(ctx, `UPDATE session_files SET offset = ? WHERE session_id = ? AND path = ?`,
				end(p), id, p); err != nil {
				return err
			}
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

// stopSleepers stops the agents of asleep sessions that are still running.
func (r *Runner) stopSleepers(ctx context.Context) error {
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT id FROM sessions WHERE state = 'asleep' AND spec != ''`)
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
		if _, err := os.Stat(filepath.Join(dir, host.HostPIDFile)); err != nil || host.Gone(dir) || hostLost(dir) {
			continue
		}
		r.Log.Warn("stopping the agent of an asleep session", "session", id)
		go host.Stop(dir, time.Second, 2*time.Second)
	}
	return nil
}

// follow starts following a session. On reconnect, the codec first reads
// what was read before, with its results discarded, to rebuild its state.
func (r *Runner) follow(id int64, ad agent.Adapter, spec agent.Spec, m mode) error {
	resumed := m == reconnect
	lv := &live{id: id, dir: spec.StateDir, adapter: ad, codec: ad.NewCodec(spec), spec: spec,
		files: map[string]int64{}, stop: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
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
	} else if !resumed {
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
		r.do(lv, r.inputActions(lv, agent.Input{ID: firstInputID, Text: f.Context + f.Text, Written: true}))
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

// warm feeds the codec what was read before the stored offsets.
// It returns the replies to hook calls it produced: one that was never sent
// because the daemon died first still has its call waiting at the host.
func (r *Runner) warm(lv *live) []agent.HookReply {
	var replies []agent.HookReply
	if b, err := os.ReadFile(filepath.Join(lv.dir, host.EventsFile)); err == nil {
		for _, l := range lines(b, lv.start, lv.offset) {
			replies = append(replies, lv.codec.Decode(source(l), l).Reply...)
		}
	}
	for p, off := range lv.files {
		if b, err := os.ReadFile(p); err == nil {
			for _, l := range lines(b, 0, off) {
				replies = append(replies, lv.codec.Decode(agent.File, l).Reply...)
			}
		}
	}
	return replies
}

// hostLost reports whether the host of dir is gone without recording an
// end: its process no longer exists, as after a host reboot.
func hostLost(dir string) bool {
	if _, ok, _ := host.ReadStatus(dir); ok {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, host.HostPIDFile))
	if err != nil {
		return false // not started yet, or never: the start check handles it
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
	if err != nil || pid <= 1 {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
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
	lastTick := time.Now()
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
	}
}

// batch is what one read of the logs produced.
type batch struct {
	events  []agent.Event
	actions []agent.Actions
	offset  int64
	files   map[string]int64
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
		return !host.Gone(lv.dir) && !hostLost(lv.dir)
	}
	exited := host.Gone(lv.dir) || hostLost(lv.dir) // looked at before reading, so nothing written before the exit is missed
	b := batch{offset: lv.offset, files: map[string]int64{}}
	if data, err := os.ReadFile(filepath.Join(lv.dir, host.EventsFile)); err == nil {
		end := complete(data, lv.offset)
		for _, l := range lines(data, lv.offset, end) {
			r.decode(lv, &b, source(l), l)
		}
		b.offset = end
	}
	for p, off := range lv.files {
		b.files[p] = off
	}
	for i := 0; i < len(lv.files)+1; i++ { // a followed file may name another
		for p, off := range b.files {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			end := complete(data, off)
			for _, l := range lines(data, off, end) {
				r.decode(lv, &b, agent.File, l)
			}
			b.files[p] = end
		}
	}
	r.watchScreen(lv, &b)
	first, err := r.commit(lv, b)
	if err != nil {
		// The codec has read lines whose results were not stored: build it
		// again from what was, so the lines are read once more by a codec
		// that has not seen them.
		r.Log.Error("commit", "session", lv.id, "err", err)
		lv.codec = lv.adapter.NewCodec(lv.spec)
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
		if e.Kind == agent.SessionEnded && e.Reason == agent.CannotResume {
			go r.replace(lv)
		}
		if e.Kind == agent.SessionEnded && e.Reason == agent.WrongSession {
			// The session failed; its agent runs another conversation.
			go host.Stop(lv.dir, time.Second, 2*time.Second)
		}
	}
	for _, a := range b.actions {
		r.act(lv, a)
	}
	if first != nil {
		a, err := lv.codec.Input(agent.Input{ID: firstInputID, Text: first.Context + first.Text})
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

// watchScreen gives the codec what the agent wrote to its terminal since
// the last step, while it watches the screen. Once the session has reported
// SessionReady, now or before, it stops for good: the terminal is not read
// again.
func (r *Runner) watchScreen(lv *live, b *batch) {
	if !lv.screen {
		return
	}
	for _, e := range b.events {
		if e.Kind == agent.SessionReady {
			lv.screen = false
			return
		}
	}
	if lv.ready {
		lv.screen = false
		return
	}
	data, next := host.Screen(lv.dir, lv.seenTo)
	lv.seenTo = next
	if len(data) > 0 {
		r.decode(lv, b, agent.Screen, data)
	}
}

// decode runs one line through the codec and keeps what it gave.
func (r *Runner) decode(lv *live, b *batch, src agent.Source, l []byte) {
	a := lv.codec.Decode(src, l)
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
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET log_offset = ? WHERE id = ?`, b.offset, lv.id); err != nil {
			return err
		}
		for p, o := range b.files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO session_files (session_id, path, offset) VALUES (?, ?, ?)
				ON CONFLICT (session_id, path) DO UPDATE SET offset = excluded.offset`, lv.id, p, o); err != nil {
				return err
			}
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
		case !f.Sent && control.State(state) == control.Idle:
			if err := r.Machine.ApplyTx(ctx, tx, lv.id, session.Input{Kind: session.Send, Principal: f.Principal, Key: f.Key, Text: f.Text}); err != nil {
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
		gone := e.Kind == agent.TurnStarted || e.Kind == agent.TurnEnded && e.Reason == agent.NotDelivered
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
		if e.Kind == agent.TurnStarted && e.Input == id {
			return true
		}
	}
	return false
}

// do commits the events of actions that did not come from a line, then
// carries out the rest. It returns the first error writing to the host.
func (r *Runner) do(lv *live, a agent.Actions) error {
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

// act writes to the agent and answers waiting hook calls. It returns the
// first error, which it has logged.
func (r *Runner) act(lv *live, a agent.Actions) error {
	if len(a.Write) == 0 && len(a.Reply) == 0 {
		return nil
	}
	if len(a.Inputs) > 0 {
		if err := r.markWritten(lv, a.Inputs); err != nil {
			// Not written: the codec, which counts it written, fails its
			// turn as "not delivered" when no report comes.
			a.Write = nil
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
	for _, w := range a.Write {
		if err := lv.client.Input(w); err != nil {
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
	st, recorded, _ := host.ReadStatus(lv.dir)
	in := session.Input{Kind: session.Exit}
	switch {
	case !recorded:
		in.Reason = session.ReasonInterrupted // the host died with it, as in a reboot (section 4.5)
	case st.Stopped:
		in.Reason = "stopped"
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
	if lv.asleep || lv.failed {
		return
	}
	slept := false
	ctx := context.Background()
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
		go host.Stop(lv.dir, time.Second, 2*time.Second)
	}
}

// replace starts a new session in the same workspace for a session whose
// conversation the agent no longer has, with the input the old one was
// woken for. It does nothing when the thread already has a newer session.
func (r *Runner) replace(lv *live) {
	ctx := context.Background()
	var (
		l     core.Launch
		first firstInput
		fi    string
		state string
	)
	err := r.Store.Update(ctx, func(tx *store.Tx) error {
		l = core.Launch{}
		var thread int64
		if err := tx.QueryRowContext(ctx, `SELECT s.state, s.thread_id, s.agent, s.model, s.effort, s.first_input, t.scope, t.key
			FROM sessions s JOIN threads t ON t.id = s.thread_id WHERE s.id = ?`, lv.id).
			Scan(&state, &thread, &l.Agent, &l.Model, &l.Effort, &fi, &l.Thread.Scope, &l.Thread.Key); err != nil {
			return err
		}
		var newer int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE thread_id = ? AND id > ?`, thread, lv.id).Scan(&newer); err != nil {
			return err
		}
		if control.State(state) != control.Failed || newer > 0 {
			return nil
		}
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `INSERT INTO sessions (thread_id, agent, state, model, effort, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, thread, l.Agent, string(control.Creating), l.Model, l.Effort, now, now)
		if err != nil {
			return err
		}
		if l.Session, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ?`,
			string(control.Creating), now, thread); err != nil {
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
	if fi != "" {
		json.Unmarshal([]byte(fi), &first)
	}
	l.Dir, l.Principal, l.Key, l.Input = lv.spec.Dir, first.Principal, first.Key, control.Input{Text: first.Text}
	if l.Context, err = r.earlier(ctx, l.Thread); err != nil {
		r.Log.Error("the thread's earlier context", "session", l.Session, "err", err) // the new session starts without it
	}
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
	if lv.client != nil {
		tail, _ = lv.client.Tail(4000)
	}
	if err := r.Machine.Apply(context.Background(), lv.id, session.Input{Kind: session.StartTimeout, Detail: string(tail)}); err != nil {
		r.Log.Error("start timeout", "session", lv.id, "err", err)
		return
	}
	lv.failed = true
	go host.Stop(lv.dir, time.Second, time.Second)
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
// first. When the turn has not ended after the grace period, every process
// of the session is stopped.
func (r *Runner) Interrupt(ctx context.Context, id int64) error {
	lv, err := r.get(id)
	if err != nil {
		return err
	}
	lv.mu.Lock()
	r.do(lv, lv.codec.Interrupt())
	lv.mu.Unlock()
	go func() {
		deadline := time.Now().Add(r.interruptGrace())
		for time.Now().Before(deadline) {
			var state string
			if r.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state) == nil &&
				control.State(state) != control.Running {
				return
			}
			time.Sleep(r.poll())
		}
		r.Log.Warn("the turn did not end after the interrupt; stopping the session", "session", id)
		host.Stop(lv.dir, time.Second, 2*time.Second)
	}()
	return nil
}
