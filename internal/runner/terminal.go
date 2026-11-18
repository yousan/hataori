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
