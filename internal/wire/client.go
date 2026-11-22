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
	token   string // presented in every hello; empty for the owner

	mu      sync.Mutex
	conn    net.Conn
	peer    *Peer
	handler Handler
	daemon  string // the version the daemon said it speaks on the connection open made
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

// DialToken connects to the daemon of the instance at root as the caller
// the daemon issued token for. Every connection the client opens, for
// calls and for streams, presents it. An unknown token is refused.
func DialToken(root, token string) (*Client, error) {
	return DialTokenHandler(root, token, nil)
}

// DialTokenHandler is DialToken with h answering the requests the daemon
// sends from the start, such as a plugin's plugin_started, which may come
// as soon as the hellos are exchanged.
func DialTokenHandler(root, token string, h Handler) (*Client, error) {
	if token == "" {
		return nil, errors.New("wire: no token")
	}
	path, err := clientPath(root)
	if err != nil {
		return nil, err
	}
	return dialHandler(path, Version, token, h)
}

func dialPath(path, version string) (*Client, error) { return dial(path, version, "") }

func dial(path, version, token string) (*Client, error) {
	return dialHandler(path, version, token, nil)
}

func dialHandler(path, version, token string, h Handler) (*Client, error) {
	c := &Client{path: path, version: version, token: token, handler: h}
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
	conn, dec, version, err := c.connect(ctx)
	if err != nil {
		return err
	}
	p := newPeer(conn, c.handle)
	c.conn, c.peer, c.daemon = conn, p, version
	go func() {
		p.serve(context.Background(), dec, nil)
		conn.Close()
	}()
	return nil
}

// connect opens a connection and exchanges hellos, and returns the
// version the daemon speaks.
func (c *Client) connect(ctx context.Context) (net.Conn, *json.Decoder, string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("wire: connect to %s: %w", c.path, err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := json.NewEncoder(conn).Encode(hello{Version: c.version, Token: c.token}); err != nil {
		conn.Close()
		return nil, nil, "", fmt.Errorf("wire: hello: %w", err)
	}
	var h hello
	if err := dec.Decode(&h); err != nil {
		conn.Close()
		return nil, nil, "", fmt.Errorf("wire: hello: %w", err)
	}
	if h.Error != "" {
		conn.Close()
		return nil, nil, "", fmt.Errorf("wire: daemon refused the connection: %s", h.Error)
	}
	if major(h.Version) != major(c.version) {
		conn.Close()
		return nil, nil, "", versionError(c.version, h.Version)
	}
	if c.token != "" && !atLeast(h.Version, tokensSince) {
		// It would make this connection the owner's.
		conn.Close()
		return nil, nil, "", fmt.Errorf("wire: the daemon speaks %s, which does not know tokens (since %s); refusing to connect as the owner", h.Version, tokensSince)
	}
	return conn, dec, h.Version, nil
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
	return c.callSince(ctx, "", "", op, args, result)
}

// callSince is call, refused without sending when the daemon of the
// connection the request goes out on speaks a version older than since;
// what names what needs it. An empty since needs nothing.
func (c *Client) callSince(ctx context.Context, since, what, op string, args, result any) error {
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
	daemon := c.daemon
	c.mu.Unlock()
	if since != "" && !atLeast(daemon, since) {
		return fmt.Errorf("wire: the daemon speaks %s; %s needs %s or later: upgrade the daemon", daemon, what, since)
	}
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

func (c *Client) ThreadEvents(ctx context.Context, id control.ThreadID, before uint64, n int) ([]control.Event, error) {
	var out []control.Event
	err := c.call(ctx, "ThreadEvents", threadEventsArgs{ID: id, Before: before, Limit: n}, &out)
	return out, err
}

func (c *Client) Scopes(ctx context.Context) ([]control.ScopeInfo, error) {
	var out []control.ScopeInfo
	err := c.call(ctx, "Scopes", scopesArgs{}, &out)
	return out, err
}

func (c *Client) SetName(ctx context.Context, l control.LinkRef, name string) error {
	return c.call(ctx, "SetName", setNameArgs{Link: l, Name: name}, nil)
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

func (c *Client) AskInterrupt(ctx context.Context, id control.ThreadID, key, reason string) (control.RequestID, error) {
	var out control.RequestID
	err := c.call(ctx, "AskInterrupt", askInterruptArgs{ID: id, Key: key, Reason: reason}, &out)
	return out, err
}

// Stop names turn, when it is not 0, only to a daemon that knows the field:
// an older one would stop whatever turn runs.
func (c *Client) Stop(ctx context.Context, id control.ThreadID, turn int64) error {
	if turn == 0 {
		return c.call(ctx, "Stop", stopArgs{ID: id}, nil)
	}
	return c.callSince(ctx, stopTurnSince, "naming the turn to stop", "Stop", stopArgs{ID: id, Turn: turn}, nil)
}

// stopTurnSince is the version since which Stop takes the turn to stop.
const stopTurnSince = "3.14"

func (c *Client) Link(ctx context.Context, id control.ThreadID, l control.LinkRef) (control.LinkRef, error) {
	var out control.LinkRef
	err := c.call(ctx, "Link", linkArgs{ID: id, Link: l}, &out)
	return out, err
}

// CloseThread is the operation Close.
func (c *Client) CloseThread(ctx context.Context, id control.ThreadID) error {
	return c.call(ctx, "Close", closeArgs{ID: id}, nil)
}

// TakeIn is the operation TakeIn. Since 3.17.
func (c *Client) TakeIn(ctx context.Context, r control.TakeInRequest) (control.ThreadID, error) {
	var out control.ThreadID
	err := c.call(ctx, "TakeIn", r, &out)
	return out, err
}

// Release is the operation Release. Since 3.17.
func (c *Client) Release(ctx context.Context, id control.ThreadID) error {
	return c.call(ctx, "Release", releaseArgs{ID: id}, nil)
}

// RemoveThread is the operation Remove.
func (c *Client) RemoveThread(ctx context.Context, id control.ThreadID) error {
	return c.call(ctx, "Remove", removeArgs{ID: id}, nil)
}

func (c *Client) Workspace(ctx context.Context, id control.ThreadID) (control.WorkspaceState, error) {
	var out control.WorkspaceState
	err := c.call(ctx, "Workspace", workspaceArgs{ID: id}, &out)
	return out, err
}

func (c *Client) SignInLink(ctx context.Context, way control.Way) (string, error) {
	// An older daemon ignores the field and answers with its one
	// listener's link, which may not be the listener asked for.
	since := ""
	if way != "" {
		since = waysSince
	}
	var out string
	err := c.callSince(ctx, since, "a sign-in link for one listener", "SignInLink", signInLinkArgs{Way: way}, &out)
	return out, err
}

// waysSince is the first version whose daemon honours SignInLink's way.
const waysSince = "3.2"

func (c *Client) HostTerminals(ctx context.Context) ([]control.HostTerminal, error) {
	var out []control.HostTerminal
	err := c.call(ctx, "HostTerminals", hostTerminalsArgs{}, &out)
	return out, err
}

// HostView needs a daemon of 3.13 or later.
func (c *Client) HostView(ctx context.Context, pane string) (control.HostView, error) {
	var out control.HostView
	err := c.callSince(ctx, viewSince, "a host pane's cursor", "HostView", hostScreenArgs{Pane: pane}, &out)
	return out, err
}

// viewSince is the first version whose daemon knows HostView.
const viewSince = "3.13"

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
		resp, err := streamResponse(dec)
		if err != nil || resp.Terminal == nil {
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
	conn, dec, _, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	raw, _ := json.Marshal(args)
	var resp response
	err = json.NewEncoder(conn).Encode(request{ID: 1, Op: op, Args: raw})
	if err == nil {
		resp, err = streamResponse(dec)
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

// streamResponse reads the next response on a stream's connection. A
// request the daemon sent before it read the stream's request, such as a
// plugin's plugin_started, is skipped unanswered: the daemon counts it as
// failed once the stream starts (docs/protocol.md).
func streamResponse(dec *json.Decoder) (response, error) {
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return response{}, err
		}
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			return response{}, err
		}
		if m.Op != "" {
			continue
		}
		var resp response
		return resp, json.Unmarshal(raw, &resp)
	}
}

// pump delivers events from one connection until it breaks or ctx ends,
// and returns the position of the last event delivered.
func (c *Client) pump(ctx context.Context, conn net.Conn, dec *json.Decoder, out chan<- control.Event, after uint64) uint64 {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for {
		resp, err := streamResponse(dec)
		if err != nil || resp.Event == nil {
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

// Clients is control.API.Clients. A daemon before 3.21 answers
// not_supported.
func (c *Client) Clients(ctx context.Context) ([]control.ClientState, error) {
	var out []control.ClientState
	err := c.call(ctx, "Clients", clientsArgs{}, &out)
	return out, err
}

// AttachTerminal is control.API.AttachTerminal. It waits as long as the
// daemon does for a thread that is being woken.
func (c *Client) AttachTerminal(ctx context.Context, id control.ThreadID) (control.Attachment, error) {
	var out control.Attachment
	err := c.call(ctx, "AttachTerminal", attachTerminalArgs{ID: id}, &out)
	return out, err
}
