package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// Terminal streams the terminal output of session id from its holder, as
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
	return r.hold().Screen(ctx, r.window(dir), run, after)
}

// Device returns the name of the terminal that session id's agent runs in,
// as its running host reports it; "" when the session runs on pipes, when
// no host runs or its command has exited, when the host does not answer in
// time, and for a host started by a daemon before protocol 1.12, which
// does not report it.
func (r *Runner) Device(ctx context.Context, id int64) string {
	dir := StateDir(r.Root, id)
	spec, ok, err := host.ReadSpec(dir)
	if err != nil || !ok || !spec.Terminal || r.alive(dir).Ended() {
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
		if ok && r.alive(dir).State != holder.Exited {
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
