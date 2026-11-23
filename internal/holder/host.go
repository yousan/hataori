package holder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
)

// HostName is the name of hataori's own session host as a holder.
const HostName = "host"

// Host is hataori's own session host as a holder: a pseudo-terminal it
// opens itself, or pipes. A window is a session's state directory, and each
// run of a host there is told apart by its run id (Screen). Host keeps no
// state of its own; everything is in the state directory.
//
// Beyond Holder it offers what only it does so far: input on pipes
// (Write), the raw screen by position for a codec watching the start
// (Since), the end of the raw log (Tail), and preparing a state directory
// whose host ended for the next one (WaitReleased, Reset).
type Host struct {
	Command host.Command  // how to run `hataori host`
	Poll    time.Duration // how often Screen looks for a new run; 50ms when zero
}

var _ Holder = (*Host)(nil)

// Window returns the window of the host in stateDir. Its Process is not
// read: Start gives it.
func (h *Host) Window(stateDir string) Window {
	return Window{Holder: HostName, Ref: stateDir, Owned: true}
}

// Caps: the host starts agents and keeps their raw output.
func (h *Host) Caps() Caps { return Caps{CanStart: true, RawScreen: true} }

// Start starts a session host in stateDir, which starts the agent once it
// listens. With spec.Again, a host found there that the daemon that died
// started is taken instead: one that runs, or one whose process id differs
// from the window recorded before.
func (h *Host) Start(ctx context.Context, stateDir string, spec Spec) (Window, error) {
	pid := hostPID(stateDir)
	switch {
	case spec.Again != nil && (pid > 0 && pid != spec.Again.Process || host.Running(stateDir)):
		// A daemon that died had started this host, which runs or has run:
		// it is followed, not started again. The old host of a woken session
		// had let go of the state directory before the start was begun.
		if err := host.WaitReady(stateDir); err != nil {
			return Window{}, err
		}
	case host.Running(stateDir):
		if spec.Again == nil {
			return Window{}, ErrHeld
		}
		// The host the daemon that died started took the lock just now.
		if err := host.WaitReady(stateDir); err != nil {
			return Window{}, err
		}
	default:
		if _, err := host.Start(h.Command, stateDir, host.Spec{Argv: spec.Argv, Env: spec.Env, Terminal: spec.Terminal,
			Dir: spec.Dir, Cols: spec.Cols, Rows: spec.Rows}); err != nil {
			return Window{}, err
		}
	}
	// The host writes its pid before it listens; a process that found the
	// state directory taken exits, so the file names the one that runs.
	w := h.Window(stateDir)
	w.Process = hostPID(stateDir)
	return w, nil
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

// Bracketed paste: what the terminal sends around pasted text.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// keys is the fixed table of the named keys, as a terminal in its normal
// mode sends them.
var keys = map[agent.Key]string{
	agent.Enter:  "\r",
	agent.Escape: "\x1b",
	agent.Up:     "\x1b[A",
	agent.Down:   "\x1b[B",
}

// Encode returns the bytes the host writes to the terminal for p: a paste
// between the bracketed-paste marks, its text unchanged, or a key from the
// fixed table. A key not in the table, or a part that is both, is an error.
func Encode(p agent.Part) ([]byte, error) {
	if p.Key == "" {
		return []byte(pasteStart + p.Paste + pasteEnd), nil
	}
	if p.Paste != "" {
		return nil, fmt.Errorf("holder: a part is both a paste and the key %q", p.Key)
	}
	b, ok := keys[p.Key]
	if !ok {
		return nil, fmt.Errorf("holder: no key %q", p.Key)
	}
	return []byte(b), nil
}

// Input writes each part to the terminal, encoded (Encode), one write each.
func (h *Host) Input(ctx context.Context, w Window, parts []agent.Part) error {
	bs := make([][]byte, len(parts))
	for i, p := range parts {
		b, err := Encode(p)
		if err != nil {
			return err
		}
		bs[i] = b
	}
	return h.Write(ctx, w, bs)
}

// Write writes each of bs to the agent's terminal or standard input,
// unchanged, one write each and in order. A write that fails does not stop
// the ones after it; the errors are returned together. Nothing is written
// when the host cannot be reached.
func (h *Host) Write(ctx context.Context, w Window, bs [][]byte) error {
	if len(bs) == 0 {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	c, err := host.DialContext(dctx, w.Ref)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	var errs []error
	for _, b := range bs {
		if err := c.Input(b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dialTimeout bounds how long a call waits to reach a host.
const dialTimeout = 5 * time.Second

// Since returns what the agent of the host's current run wrote to its
// terminal after position from, counted from that run's first byte, and
// the position to ask from next: for a codec watching the start
// (agent.Actions.WatchScreen). Positions do not change when the host trims
// its log. On pipes, and once the host has gone, there is nothing.
func (h *Host) Since(w Window, from uint64) ([]byte, uint64) {
	return host.Screen(w.Ref, from)
}

// Tail returns the last n bytes of the raw log (the terminal, or standard
// error on pipes), for a person to read.
func (h *Host) Tail(ctx context.Context, w Window, n int) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	c, err := host.DialContext(dctx, w.Ref)
	cancel()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Tail(n)
}

// Stop ends the agent's process group: SIGINT, then SIGTERM after
// interrupt, then SIGKILL after terminate. It returns once the host has
// recorded how the agent ended. ctx is not used: a stop that has begun is
// finished.
func (h *Host) Stop(ctx context.Context, w Window, interrupt, terminate time.Duration) (StopResult, error) {
	if !w.Owned {
		return StopResult{}, ErrNotOwned
	}
	return host.Stop(w.Ref, interrupt, terminate)
}

// Alive reads the state directory: Exited once the host recorded the end and
// its socket is gone, Gone when its process no longer exists and recorded
// none (as after a reboot), Unknown before a host recorded its process id,
// and Running otherwise. It never takes the host's lock, which could make a
// host that is starting give up.
func (h *Host) Alive(ctx context.Context, w Window) (Liveness, error) {
	dir := w.Ref
	var l Liveness
	if st, ok, _ := host.ReadStatus(dir); ok {
		l.Status = &st
	}
	switch {
	case host.Gone(dir):
		l.State = Exited
	case lost(dir):
		l.State = Gone
	case !pidFile(dir):
		l.State = Unknown
	default:
		l.State = Running
	}
	return l, nil
}

// pidFile reports whether the host of dir recorded its process id.
func pidFile(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, host.HostPIDFile))
	return err == nil
}

// lost reports whether the host of dir is gone without recording an end:
// its process no longer exists, as after a host reboot.
func lost(dir string) bool {
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

// WaitReleased waits, up to d, until a host whose agent ended has let go of
// the state directory: it records the end before it does. It reports
// whether it did.
func (h *Host) WaitReleased(w Window, d time.Duration) bool {
	for deadline := time.Now().Add(d); host.Running(w.Ref); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// Reset forgets how the last agent in the window ended, before another
// starts there: its recorded status would make the new one look ended.
func (h *Host) Reset(w Window) error {
	if err := os.Remove(filepath.Join(w.Ref, host.StatusFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (h *Host) poll() time.Duration {
	if h.Poll > 0 {
		return h.Poll
	}
	return 50 * time.Millisecond
}

// Screen streams the window's raw terminal output, following the host
// across runs: when the session sleeps and wakes, the stream goes on with
// the new run. After a host ended, the end of the log it kept is sent once,
// with Reset, and nothing more until a host runs again. The stream ends when
// the state directory turns out to hold an agent on pipes. It only reads;
// nothing goes to the terminal.
func (h *Host) Screen(ctx context.Context, w Window, run string, after uint64) (<-chan control.TerminalChunk, error) {
	out := make(chan control.TerminalChunk)
	go h.follow(ctx, w.Ref, run, after, out)
	return out, nil
}

func (h *Host) follow(ctx context.Context, dir, run string, after uint64, out chan<- control.TerminalChunk) {
	defer close(out)
	send := func(ch control.TerminalChunk) bool {
		select {
		case out <- ch:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// shown: the reader has the end of the log of a host that has ended,
	// so nothing is sent until a host runs again.
	shown := false
	// sent: a chunk was sent, last at that size. After the first, a chunk
	// that adds nothing (a stream that started again where the last one
	// stopped) is not; one with a new size alone is: the terminal was
	// resized there.
	sent := false
	var cols, rows int
	send0 := send
	send = func(ch control.TerminalChunk) bool {
		if sent && !ch.Reset && len(ch.Data) == 0 && ch.Cols == cols && ch.Rows == rows {
			return true
		}
		sent, cols, rows = true, ch.Cols, ch.Rows
		return send0(ch)
	}
	for ctx.Err() == nil {
		spec, ok, _ := host.ReadSpec(dir)
		if ok && !spec.Terminal {
			return
		}
		if ok {
			if s, err := host.FollowTerminal(dir, run, after); err == nil {
				ended, alive := pump(ctx, s, send, &run, &after)
				if !alive {
					return
				}
				if !ended {
					continue // at once: a reader cut off goes on from its position
				}
				// The run's terminal closed and everything was sent. The host
				// may still answer while it finishes; wait for the next run.
				shown = true
			} else if !shown && (host.Gone(dir) || lost(dir)) {
				// Gone or lost, never Running: taking the host's lock could
				// make a host that is starting give up.
				cols, rows := spec.Size()
				if !send(control.TerminalChunk{Reset: true, Cols: int(cols), Rows: int(rows), Data: host.KeptTerminal(dir)}) {
					return
				}
				shown, run, after = true, "", 0
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.poll()):
		}
	}
}

// pump passes on the chunks of one host stream and records where they got
// to. ended is true when the host's terminal closed and everything was
// sent; alive is false when the reader is gone.
func pump(ctx context.Context, s *host.TerminalStream, send func(control.TerminalChunk) bool, run *string, after *uint64) (ended, alive bool) {
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	for {
		ch, err := s.Next()
		if err != nil {
			return errors.Is(err, io.EOF), ctx.Err() == nil
		}
		if !send(control.TerminalChunk{Run: ch.Run, Pos: ch.Pos, Reset: ch.Reset, Cols: ch.Cols, Rows: ch.Rows, Data: ch.Data}) {
			return false, false
		}
		*run, *after = ch.Run, ch.Pos
	}
}
