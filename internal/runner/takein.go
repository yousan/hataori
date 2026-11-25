package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/holder/tmux"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/window"
)

// TakenFile, in a session's state directory, names the window a person
// started that the session was taken in from (design, section 4.8). While
// it is there, the session's terminal is that window's: input goes to it
// through its holder, and nothing hataori does ends its process.
const TakenFile = "taken-in.json"

// takenIn is what TakenFile holds.
type takenIn struct {
	Window holder.Window `json:"window"` // the holder's window, with the agent's process
	Server string        `json:"server"` // as window.Report.Server, for the window's record
	Pane   string        `json:"pane"`
}

// takenOf returns the window the session in dir was taken in from; ok is
// false for a session hataori's own host holds.
func takenOf(dir string) (takenIn, bool) {
	b, err := os.ReadFile(filepath.Join(dir, TakenFile))
	if err != nil {
		return takenIn{}, false
	}
	var t takenIn
	if json.Unmarshal(b, &t) != nil || t.Window.Holder == "" {
		return takenIn{}, false
	}
	return t, true
}

// tmuxHolder is the holder of tmux windows people started.
func (r *Runner) tmuxHolder() *tmux.Holder {
	if r.Tmux != nil {
		return r.Tmux
	}
	return &tmux.Holder{}
}

// reportWindow is the holder's window that rep names.
func reportWindow(rep window.Report) (holder.Window, error) {
	if rep.Holder != window.Tmux {
		return holder.Window{}, fmt.Errorf("runner: no holder %q for a window", rep.Holder)
	}
	// TMUX names the session too; the window does not need it.
	return tmux.Window(rep.Server+",0", rep.Pane, rep.PID)
}

// CheckTakeIn asks the window's holder whether the window rep names may be
// taken in: it exists, and the agent's process that reported runs on its
// terminal as its foreground process, on a server that is not too old
// (design, section 4.8).
func (r *Runner) CheckTakeIn(ctx context.Context, rep window.Report) error {
	w, err := reportWindow(rep)
	if err != nil {
		return err
	}
	return r.tmuxHolder().CheckTakeIn(ctx, w)
}

// TakenIn reports whether the window rep names is taken in now.
func (r *Runner) TakenIn(rep window.Report) (bool, error) {
	_, ok, err := window.ReadRecord(r.Root, rep.Server, rep.Pane)
	return ok, err
}

// TakeIn follows the new session l, made for the window rep names, which a
// person started: no agent is launched. A relay host with no command takes
// the window's hook calls (section 4.3), the window's record sends them
// there, and the session's codec starts ready (agent.Spec.TakenIn).
func (r *Runner) TakeIn(ctx context.Context, l core.Launch, rep window.Report) error {
	ad, ok := r.Adapters[l.Agent]
	if !ok {
		return fmt.Errorf("runner: no agent %q", l.Agent)
	}
	w, err := reportWindow(rep)
	if err != nil {
		return err
	}
	dir := StateDir(r.Root, l.Session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	// The configuration directory the window used is the one its session
	// resumes with under hataori's own host, after the window closes.
	spec := agent.Spec{Dir: rep.Dir, StateDir: dir, ResumeID: rep.Session, Model: rep.Model,
		Approvals: agent.AllowAll, TakenIn: true, ConfigDir: rep.ConfigDir}
	sb, _ := json.Marshal(storedSpec{Agent: l.Agent, Spec: spec})
	fb, _ := json.Marshal(firstInput{Principal: l.Principal, Dir: rep.Dir, Wake: true, NoInput: true})
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET spec = ?, first_input = ? WHERE id = ?`,
		string(sb), string(fb), l.Session); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	pid, err := host.Start(r.Host, dir, host.Spec{Relay: true})
	if err != nil {
		return err
	}
	// Each step undone on a failure, so that nothing points at the window
	// for a session that failed.
	t := takenIn{Window: w, Server: rep.Server, Pane: rep.Pane}
	fail := func(err error) error {
		if lerr := r.letGo(ctx, l.Session, dir, t, nil); lerr != nil {
			r.Log.Error("undoing a failed take-in", "session", l.Session, "err", lerr)
		}
		return err
	}
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET host_pid = ?, launch = 0 WHERE id = ?`, pid, l.Session); err != nil {
		return fail(fmt.Errorf("runner: %w", err))
	}
	tb, _ := json.Marshal(t)
	if err := writeAtomic(filepath.Join(dir, TakenFile), tb); err != nil {
		return fail(err)
	}
	if err := window.WriteRecord(r.Root, window.Record{Server: rep.Server, Pane: rep.Pane, PID: rep.PID,
		Socket: host.SocketPath(dir), Thread: int64(l.Thread), Session: l.Session}); err != nil {
		return fail(err)
	}
	if err := r.follow(l.Session, ad, spec, fresh); err != nil {
		return fail(err)
	}
	return nil
}

// ErrNotTakenIn is returned by Release for a session that was not taken in.
var ErrNotTakenIn = errors.New("runner: the session was not taken in from a window")

// Release ends a take-in (design, section 4.8): the session goes asleep, as
// an idle one does, its window's record and its relay host go, and the
// window's hooks only report again. The window is not touched. A session
// that is not idle is refused, and nothing changes.
func (r *Runner) Release(ctx context.Context, id int64) error {
	dir := StateDir(r.Root, id)
	t, ok := takenOf(dir)
	if !ok {
		return fmt.Errorf("runner: session %d: %w", id, ErrNotTakenIn)
	}
	r.mu.Lock()
	lv := r.live[id]
	r.mu.Unlock()
	apply := func() error {
		return r.Store.Update(ctx, func(tx *store.Tx) error {
			// As Shut: input or an answer accepted and not reported by a turn
			// yet would be lost without a trace.
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_inputs WHERE session_id = ?`, id).Scan(&pending); err != nil {
				return fmt.Errorf("runner: session %d: %w", id, err)
			}
			if pending > 0 {
				return fmt.Errorf("runner: session %d: %d pending inputs not reported by a turn: %w", id, pending, session.ErrRefused)
			}
			return r.Machine.ApplyTx(ctx, tx, id, session.Input{Kind: session.IdleTimeout})
		})
	}
	if lv != nil {
		lv.mu.Lock()
		err := apply()
		if err == nil {
			lv.asleep = true
		}
		lv.mu.Unlock()
		if err != nil {
			return err
		}
	} else if err := apply(); err != nil {
		return err
	}
	return r.letGo(ctx, id, dir, t, lv)
}

// letGo removes the window's record, then the session's own note of it,
// and stops the relay host: the window's hooks only report from then on.
func (r *Runner) letGo(ctx context.Context, id int64, dir string, t takenIn, lv *live) error {
	// Only this session's record: the window may have been taken in again
	// by another, after this one failed.
	if rec, ok, err := window.ReadRecord(r.Root, t.Server, t.Pane); err != nil {
		return err
	} else if ok && rec.Session == id {
		if err := window.RemoveRecord(r.Root, t.Server, t.Pane); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, TakenFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runner: %w", err)
	}
	if r.alive(dir).State == holder.Running {
		if _, err := r.stop(dir, time.Second, time.Second); err != nil {
			return fmt.Errorf("runner: session %d: stopping its relay host: %w", id, err)
		}
	}
	if lv != nil {
		select {
		case <-lv.done:
		case <-time.After(shutWait):
			return fmt.Errorf("runner: session %d: its following has not ended", id)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// writeAtomic writes b to p whole or not at all.
func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("runner: %w", err)
	}
	return nil
}

var _ core.TakeInRunner = (*Runner)(nil)

// creating reports whether session id is still being created.
func (r *Runner) creating(id int64) bool {
	var state string
	r.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state)
	return state == "creating"
}

// clearReason is the reason of a SessionEnd that a /clear in the window
// reports: the agent goes on, under a new session id.
const clearReason agent.Reason = "clear"

// windowCheck is how often the holder is asked whether the window of a
// session taken in is still there.
const windowCheck = 2 * time.Second

// checkWindow asks the window's holder whether the agent taken in still
// runs there. An agent that ended with no hook, as one killed hard, or a
// window that went, closes the window (design, section 4.8). An answer
// that cannot tell changes nothing.
func (r *Runner) checkWindow(lv *live) {
	lv.mu.Lock()
	asleep := lv.asleep
	lv.mu.Unlock()
	if asleep {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if l, err := r.tmuxHolder().Alive(ctx, lv.taken.Window); err == nil && l.Ended() {
		r.windowClosed(lv)
	}
}

// windowClosed puts a session taken in asleep with the reason "window
// closed", and lets its window go: its record and its relay host go. Its
// next input resumes it under hataori's own host (resume).
func (r *Runner) windowClosed(lv *live) {
	ctx := context.Background()
	lv.mu.Lock()
	r.mu.Lock()
	current := r.live[lv.id] == lv
	r.mu.Unlock()
	select {
	case <-lv.done:
		current = false // its following ended: a later run is not this one's
	default:
	}
	if lv.asleep || !current {
		lv.mu.Unlock()
		return
	}
	err := r.Machine.Apply(ctx, lv.id, session.Input{Kind: session.Exit, Reason: session.ReasonWindowClosed})
	if err != nil && !errors.Is(err, session.ErrRefused) {
		r.Log.Error("a window taken in closed", "session", lv.id, "err", err)
		lv.mu.Unlock()
		return
	}
	lv.asleep = true // nothing more is read: the relay host goes
	lv.mu.Unlock()
	r.Log.Info("the window of a session taken in closed", "session", lv.id)
	if err := r.letGo(ctx, lv.id, lv.dir, *lv.taken, nil); err != nil {
		r.Log.Error("letting a closed window go", "session", lv.id, "err", err)
	}
}

// RemoveState removes the state directory of session id, which no host
// runs in: what hataori keeps of the session, never its workspace. It is
// for a thread taken in, whose workspace is the person's own (design,
// section 4.8): removing the thread removes this and nothing else.
func (r *Runner) RemoveState(ctx context.Context, id int64) error {
	r.mu.Lock()
	lv := r.live[id]
	r.mu.Unlock()
	if lv != nil {
		return fmt.Errorf("runner: session %d is still followed: %w", id, session.ErrRefused)
	}
	dir := StateDir(r.Root, id)
	st, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("runner: session %d: %s is not a directory", id, dir)
	}
	// A host that ended records its end before it lets go of the
	// directory: one that has just ended is waited for, briefly.
	if r.alive(dir).State == holder.Running || !r.own().WaitReleased(r.window(dir), 5*time.Second) {
		return fmt.Errorf("runner: session %d: a host still runs in its state directory: %w", id, session.ErrRefused)
	}
	// Never the session's workspace, nor a directory that holds it or lies
	// in it, as when a person ran their agent in the instance's root.
	var specJSON string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT spec FROM sessions WHERE id = ?`, id).Scan(&specJSON); err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	var s storedSpec
	if specJSON != "" {
		if err := json.Unmarshal([]byte(specJSON), &s); err != nil {
			return fmt.Errorf("runner: session %d: spec: %w", id, err)
		}
	}
	if s.Spec.Dir != "" && nested(dir, s.Spec.Dir) {
		return fmt.Errorf("runner: session %d: its state directory and its workspace %s hold one another; neither is removed: %w",
			id, s.Spec.Dir, session.ErrRefused)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("runner: session %d: %w", id, err)
	}
	return nil
}

// nested reports whether a and b are the same directory or one lies in the
// other, with links resolved; true when that cannot be told.
func nested(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return !errors.Is(err2, fs.ErrNotExist) || err1 != nil // a workspace that is gone is not in the way
	}
	in := func(x, y string) bool {
		rel, err := filepath.Rel(y, x)
		return err == nil && (rel == "." || filepath.IsLocal(rel))
	}
	return in(ra, rb) || in(rb, ra)
}
