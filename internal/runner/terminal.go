package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
)

// Terminal streams the terminal output of session id from its host, as
// control.API.Terminal says. It follows the host across runs: when the
// session sleeps and wakes, the stream goes on with the new run. It only
// reads; nothing goes to the terminal.
func (r *Runner) Terminal(ctx context.Context, id int64, run string, after uint64) (<-chan control.TerminalChunk, error) {
	dir := StateDir(r.Root, id)
	spec, ok, err := host.ReadSpec(dir)
	if err != nil {
		return nil, fmt.Errorf("runner: session %d: %w", id, err)
	}
	if ok && !spec.Terminal {
		return nil, fmt.Errorf("runner: session %d runs on pipes and has no terminal: %w", id, core.ErrNotFound)
	}
	out := make(chan control.TerminalChunk)
	go r.followTerminal(ctx, dir, run, after, out)
	return out, nil
}

// Device returns the name of the terminal that session id's agent runs in,
// as its running host reports it; "" when the session runs on pipes, when
// no host runs or its command has exited, when the host does not answer in
// time, and for a host started by a daemon before protocol 1.12, which
// does not report it.
func (r *Runner) Device(ctx context.Context, id int64) string {
	dir := StateDir(r.Root, id)
	spec, ok, err := host.ReadSpec(dir)
	if err != nil || !ok || !spec.Terminal || host.Gone(dir) || hostLost(dir) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	c, err := host.DialContext(ctx, dir)
	if err != nil {
		return ""
	}
	defer c.Close()
	d, err := c.Device(ctx)
	if err != nil {
		return ""
	}
	return d
}

// deviceTimeout bounds how long Device waits for a host, so that a host
// that does not answer cannot hold up a list of threads.
const deviceTimeout = time.Second

func (r *Runner) followTerminal(ctx context.Context, dir, run string, after uint64, out chan<- control.TerminalChunk) {
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
	// sent: a chunk was sent. After the first, a chunk that adds nothing
	// (a stream that started again where the last one stopped) is not.
	sent := false
	send0 := send
	send = func(ch control.TerminalChunk) bool {
		if sent && !ch.Reset && len(ch.Data) == 0 {
			return true
		}
		sent = true
		return send0(ch)
	}
	for ctx.Err() == nil {
		spec, ok, _ := host.ReadSpec(dir)
		if ok && !spec.Terminal {
			return
		}
		if ok {
			if s, err := host.FollowTerminal(dir, run, after); err == nil {
				ended, alive := r.pumpTerminal(ctx, s, send, &run, &after)
				if !alive {
					return
				}
				if !ended {
					continue // at once: a reader cut off goes on from its position
				}
				// The run's terminal closed and everything was sent. The host
				// may still answer while it finishes; wait for the next run.
				shown = true
			} else if !shown && (host.Gone(dir) || hostLost(dir)) {
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
		case <-time.After(r.poll()):
		}
	}
}

// pumpTerminal passes on the chunks of one host stream and records where
// they got to. ended is true when the host's terminal closed and everything
// was sent; alive is false when the reader is gone.
func (r *Runner) pumpTerminal(ctx context.Context, s *host.TerminalStream, send func(control.TerminalChunk) bool, run *string, after *uint64) (ended, alive bool) {
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

// OnPipes reports whether session id's agent is known to run on pipes: the
// spec its last host was given says so.
func (r *Runner) OnPipes(id int64) bool {
	spec, ok, err := host.ReadSpec(StateDir(r.Root, id))
	return err == nil && ok && !spec.Terminal
}

// AttachSocket returns the socket of session id's host once it answers,
// for a person's terminal to attach to (host.Attach). It waits, until ctx
// ends, for a host that is being started.
func (r *Runner) AttachSocket(ctx context.Context, id int64) (string, error) {
	dir := StateDir(r.Root, id)
	for {
		spec, ok, err := host.ReadSpec(dir)
		if err != nil {
			return "", fmt.Errorf("runner: session %d: %w", id, err)
		}
		if ok && !spec.Terminal {
			return "", fmt.Errorf("runner: session %d runs on pipes: %w", id, core.ErrNoTerminal)
		}
		if ok && !host.Gone(dir) {
			dctx, cancel := context.WithTimeout(ctx, deviceTimeout)
			c, err := host.DialContext(dctx, dir)
			if err == nil {
				_, err = c.Device(dctx)
				c.Close()
			}
			cancel()
			if err == nil {
				return host.SocketPath(dir), nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("runner: session %d: its host does not answer: %w", id, ctx.Err())
		case <-time.After(r.poll()):
		}
	}
}
