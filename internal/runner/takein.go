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
// terminal as its foreground process (design, section 4.8).
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
	tb, _ := json.Marshal(takenIn{Window: w, Server: rep.Server, Pane: rep.Pane})
	if err := writeAtomic(filepath.Join(dir, TakenFile), tb); err != nil {
		return err
	}
	spec := agent.Spec{Dir: rep.Dir, StateDir: dir, ResumeID: rep.Session, Model: rep.Model,
		Approvals: agent.AllowAll, TakenIn: true}
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
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE sessions SET host_pid = ?, launch = 0 WHERE id = ?`, pid, l.Session); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	if err := window.WriteRecord(r.Root, window.Record{Server: rep.Server, Pane: rep.Pane, PID: rep.PID,
		Socket: host.SocketPath(dir), Thread: int64(l.Thread), Session: l.Session}); err != nil {
		host.Stop(dir, time.Second, time.Second)
		return err
	}
	return r.follow(l.Session, ad, spec, fresh)
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
	if err := window.RemoveRecord(r.Root, t.Server, t.Pane); err != nil {
		return err
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
