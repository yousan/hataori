package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Client is the control API of a daemon, reached over its socket. It is
// safe for concurrent use.
type Client struct {
	path    string
	version string // the version it says it speaks; Version unless a test changes it

	mu      sync.Mutex
	conn    net.Conn
	peer    *Peer
	handler Handler
}

var _ control.API = (*Client)(nil)

// Dial connects to the daemon of the instance at root.
func Dial(root string) (*Client, error) {
	path, err := clientPath(root)
	if err != nil {
		return nil, err
	}
	return dialPath(path, Version)
}

func dialPath(path, version string) (*Client, error) {
	c := &Client{path: path, version: version}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.open(context.Background()); err != nil {
		return nil, err
	}
	return c, nil
}

// SetHandler sets what answers the requests the daemon sends this client.
// Without one, every request is answered "not supported".
func (c *Client) SetHandler(h Handler) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

func (c *Client) handle(ctx context.Context, op string, args json.RawMessage) (any, error) {
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h == nil {
		return nil, fmt.Errorf("wire: operation %q: %w", op, ErrNotSupported)
	}
	return h(ctx, op, args)
}

// open connects the client's connection for calls; c.mu is held.
func (c *Client) open(ctx context.Context) error {
	conn, dec, err := c.connect(ctx)
	if err != nil {
		return err
	}
	p := newPeer(conn, c.handle)
	c.conn, c.peer = conn, p
	go func() {
		p.serve(context.Background(), dec, nil)
		conn.Close()
	}()
	return nil
}

// connect opens a connection and exchanges hellos.
func (c *Client) connect(ctx context.Context) (net.Conn, *json.Decoder, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, nil, fmt.Errorf("wire: connect to %s: %w", c.path, err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := json.NewEncoder(conn).Encode(hello{Version: c.version}); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("wire: hello: %w", err)
	}
	var h hello
	if err := dec.Decode(&h); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("wire: hello: %w", err)
	}
	if h.Error != "" {
		conn.Close()
		return nil, nil, fmt.Errorf("wire: daemon refused the connection: %s", h.Error)
	}
	if major(h.Version) != major(c.version) {
		conn.Close()
		return nil, nil, versionError(c.version, h.Version)
	}
	return conn, dec, nil
}

// Close closes the client's connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// call sends one request and waits for its response. A connection that
// broke is opened again for the next call; the call itself is not
// repeated, because the daemon may have run it.
func (c *Client) call(ctx context.Context, op string, args, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.peer != nil {
		select {
		case <-c.peer.done:
			c.peer = nil
		default:
		}
	}
	if c.peer == nil {
		if err := c.open(ctx); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	p := c.peer
	c.mu.Unlock()
	err := p.Request(ctx, op, args, result)
	var re *remoteError
	if err != nil && !errors.As(err, &re) && ctx.Err() == nil {
		return fmt.Errorf("wire: %s: %w", op, err)
	}
	return err
}

func (c *Client) Threads(ctx context.Context, f control.Filter) ([]control.ThreadInfo, error) {
	var out []control.ThreadInfo
	err := c.call(ctx, "Threads", threadsArgs{Filter: f}, &out)
	return out, err
}

func (c *Client) Thread(ctx context.Context, id control.ThreadID, recent, events int) (control.ThreadDetail, error) {
	var out control.ThreadDetail
	err := c.call(ctx, "Thread", threadArgs{ID: id, Recent: recent, Events: events}, &out)
	return out, err
}

func (c *Client) Start(ctx context.Context, r control.StartRequest) (control.ThreadID, error) {
	var out control.ThreadID
	err := c.call(ctx, "Start", r, &out)
	return out, err
}

func (c *Client) Send(ctx context.Context, id control.ThreadID, key string, in control.Input, busy control.Busy) error {
	return c.call(ctx, "Send", sendArgs{ID: id, Key: key, Input: in, Busy: busy}, nil)
}

func (c *Client) Answer(ctx context.Context, req control.RequestID, a control.Answer) error {
	return c.call(ctx, "Answer", answerArgs{Request: req, Answer: a}, nil)
}

func (c *Client) Stop(ctx context.Context, id control.ThreadID) error {
	return c.call(ctx, "Stop", stopArgs{ID: id}, nil)
}

func (c *Client) Workspace(ctx context.Context, id control.ThreadID) (control.WorkspaceState, error) {
	var out control.WorkspaceState
	err := c.call(ctx, "Workspace", workspaceArgs{ID: id}, &out)
	return out, err
}

func (c *Client) SignInLink(ctx context.Context) (string, error) {
	var out string
	err := c.call(ctx, "SignInLink", signInLinkArgs{}, &out)
	return out, err
}

func (c *Client) HostTerminals(ctx context.Context) ([]control.HostTerminal, error) {
	var out []control.HostTerminal
	err := c.call(ctx, "HostTerminals", hostTerminalsArgs{}, &out)
	return out, err
}

func (c *Client) HostScreen(ctx context.Context, pane string) (string, error) {
	var out string
	err := c.call(ctx, "HostScreen", hostScreenArgs{Pane: pane}, &out)
	return out, err
}

// Events follows the daemon's events on a connection of its own. When the
// connection breaks, it connects again and resumes after the last event it
// delivered, so a reader sees no gap and no repeat. The first request is
// made before Events returns, so a refusal is returned as an error.
func (c *Client) Events(ctx context.Context, f control.Filter, after uint64) (<-chan control.Event, error) {
	conn, dec, err := c.openStream(ctx, "Events", eventsArgs{Filter: f, After: after})
	if err != nil {
		return nil, err
	}
	out := make(chan control.Event)
	go func() {
		defer close(out)
		for {
			after = c.pump(ctx, conn, dec, out, after)
			if ctx.Err() != nil {
				return
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				if conn, dec, err = c.openStream(ctx, "Events", eventsArgs{Filter: f, After: after}); err == nil {
					break
				}
				var re *remoteError
				if errors.As(err, &re) { // the daemon said no; trying again will not help
					return
				}
			}
		}
	}()
	return out, nil
}

// Terminal follows a thread's terminal output on a connection of its own.
// When the connection breaks, it connects again and asks from the last
// run and position it delivered, so a reader gets no byte twice and none
// missing, or a chunk marked Reset when those are no longer kept. The
// first request is made before Terminal returns, so a refusal is returned
// as an error. It sends nothing to the terminal.
func (c *Client) Terminal(ctx context.Context, id control.ThreadID, run string, after uint64) (<-chan control.TerminalChunk, error) {
	conn, dec, err := c.openStream(ctx, "Terminal", terminalArgs{ID: id, Run: run, After: after})
	if err != nil {
		return nil, err
	}
	out := make(chan control.TerminalChunk)
	go func() {
		defer close(out)
		for {
			run, after = c.pumpTerminal(ctx, conn, dec, out, run, after)
			if ctx.Err() != nil {
				return
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				if conn, dec, err = c.openStream(ctx, "Terminal", terminalArgs{ID: id, Run: run, After: after}); err == nil {
					break
				}
				var re *remoteError
				if errors.As(err, &re) {
					return
				}
			}
		}
	}()
	return out, nil
}

// pumpTerminal delivers chunks from one connection until it breaks or ctx
// ends, and returns the run and position of the last chunk delivered.
func (c *Client) pumpTerminal(ctx context.Context, conn net.Conn, dec *json.Decoder, out chan<- control.TerminalChunk, run string, after uint64) (string, uint64) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for {
		var resp response
		if err := dec.Decode(&resp); err != nil || resp.Terminal == nil {
			return run, after
		}
		select {
		case out <- *resp.Terminal:
			run, after = resp.Terminal.Run, resp.Terminal.Pos
		case <-ctx.Done():
			return run, after
		}
	}
}

func (c *Client) openStream(ctx context.Context, op string, args any) (net.Conn, *json.Decoder, error) {
	conn, dec, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	raw, _ := json.Marshal(args)
	var resp response
	err = json.NewEncoder(conn).Encode(request{ID: 1, Op: op, Args: raw})
	if err == nil {
		err = dec.Decode(&resp)
	}
	if err == nil && resp.Error != nil {
		err = decodeError(resp.Error)
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, dec, nil
}

// pump delivers events from one connection until it breaks or ctx ends,
// and returns the position of the last event delivered.
func (c *Client) pump(ctx context.Context, conn net.Conn, dec *json.Decoder, out chan<- control.Event, after uint64) uint64 {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for {
		var resp response
		if err := dec.Decode(&resp); err != nil || resp.Event == nil {
			return after
		}
		if resp.Event.Kind == control.CaughtUp {
			select {
			case out <- *resp.Event: // after a reconnect too: the stored events are delivered again up to here
			case <-ctx.Done():
				return after
			}
			continue
		}
		if resp.Event.Pos <= after {
			continue
		}
		if _, known := control.EventKinds[resp.Event.Kind]; !known {
			after = resp.Event.Pos // a kind of a newer daemon: skipped, and not asked for again
			continue
		}
		select {
		case out <- *resp.Event:
			after = resp.Event.Pos
		case <-ctx.Done():
			return after
		}
	}
}

func (c *Client) Limits(ctx context.Context) ([]control.AgentLimits, error) {
	var out []control.AgentLimits
	err := c.call(ctx, "Limits", limitsArgs{}, &out)
	return out, err
}
