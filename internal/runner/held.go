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

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// HolderTmux is the value of Runner.Holder that starts the agents of
// sessions in a terminal in tmux windows of hataori's own (design, section
// 4.3).
const HolderTmux = "tmux"

// HeldFile, in a session's state directory, names the tmux window hataori
// started the session's agent in. While it is there, the session's agent is
// that window's, and hataori's own host in the directory is the relay of its
// hook calls only.
const HeldFile = "held.json"

// heldOf returns the tmux window hataori started the agent of the session
// in dir in; ok is false for an agent hataori's own host holds.
func heldOf(dir string) (holder.Window, bool) {
	b, err := os.ReadFile(filepath.Join(dir, HeldFile))
	if err != nil {
		return holder.Window{}, false
	}
	var w holder.Window
	if json.Unmarshal(b, &w) != nil || w.Holder == "" {
		return holder.Window{}, false
	}
	return w, true
}

// startHeld starts the agent of spec in a tmux window (section 4.3, "tmux",
// steps 1 to 4): the relay host with no command first, so that the agent's
// first hook call finds its socket; then the window, whose holder executes
// the spec. A daemon that died during this start had begun it (again): a
// window it made that still runs is followed, not made again.
func (r *Runner) startHeld(ctx context.Context, dir string, hs holder.Spec) (holder.Window, error) {
	if w, ok := heldOf(dir); ok {
		if hs.Again != nil && r.alive(dir).State == holder.Running {
			return w, nil
		}
		if r.alive(dir).State == holder.Running {
			if _, err := r.tmuxHolder().Stop(ctx, w, time.Second, time.Second); err != nil {
				return holder.Window{}, fmt.Errorf("runner: a window of an earlier run still runs: %w", err)
			}
		}
		r.endHeld(ctx, dir, w) // a window of an earlier run
		if _, still := heldOf(dir); still {
			return holder.Window{}, fmt.Errorf("runner: the window of an earlier run could not be let go")
		}
	}
	if _, err := host.Start(r.Host, dir, host.Spec{Relay: true}); err != nil {
		return holder.Window{}, err
	}
	w, err := r.tmuxHolder().Start(ctx, dir, hs)
	if err != nil {
		host.Stop(dir, time.Second, time.Second)
		return holder.Window{}, err
	}
	b, _ := json.Marshal(w)
	if err := writeAtomic(filepath.Join(dir, HeldFile), b); err != nil {
		r.tmuxHolder().Stop(context.WithoutCancel(ctx), w, time.Second, time.Second)
		host.Stop(dir, time.Second, time.Second)
		return holder.Window{}, err
	}
	return w, nil
}

// endHeld lets the window w of a held session go once its agent has ended:
// how the agent ended is recorded from the dead pane, the window and the
// relay host go, and so does HeldFile. Afterwards the session is as any
// whose host ended: its next launch chooses a holder again. It does
// nothing when HeldFile names another window by now, as one a later launch
// made; and when the window cannot be removed, HeldFile stays, so that the
// next start or restart tries again.
func (r *Runner) endHeld(ctx context.Context, dir string, w holder.Window) {
	if now, ok := heldOf(dir); !ok || now.Ref != w.Ref {
		return
	}
	if err := r.tmuxHolder().Record(ctx, w); err != nil {
		r.Log.Error("recording a held window's end", "dir", dir, "err", err)
		return
	}
	if now, ok := heldOf(dir); !ok || now.Ref != w.Ref {
		return
	}
	if _, err := host.Stop(dir, time.Second, time.Second); err != nil {
		r.Log.Error("stopping a held window's relay host", "dir", dir, "err", err)
	}
	if err := os.Remove(filepath.Join(dir, HeldFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.Log.Error("removing a held window's note", "dir", dir, "err", err)
	}
}

// heldTail is the end of a held window's raw log, for a person.
func heldTail(dir string, n int) []byte {
	b := host.KeptTerminal(dir)
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return b
}

// heldScreen is what the agent of a held window wrote after position from.
func heldScreen(dir string, from uint64) ([]byte, uint64) { return host.PipeSince(dir, from) }

// errHeldAttach refuses to attach a terminal to a held window: tmux holds
// it, and `tmux attach` reaches it.
var errHeldAttach = fmt.Errorf("the agent runs in a tmux window of hataori's own; attach to it with tmux")
