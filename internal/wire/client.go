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

	mu   sync.Mutex
	conn net.Conn
	dec  *json.Decoder
	next uint64
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
	conn, dec, err := c.connect()
	if err != nil {
		return nil, err
	}
	c.conn, c.dec = conn, dec
	return c, nil
}

// connect opens a connection and exchanges hellos.
func (c *Client) connect() (net.Conn, *json.Decoder, error) {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return nil, nil, fmt.Errorf("wire: connect to %s: %w", c.path, err)
	}
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
	if h.Version != c.version {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		conn, dec, err := c.connect()
		if err != nil {
			return err
		}
		c.conn, c.dec = conn, dec
	}
	c.next++
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(d)
	} else {
		c.conn.SetDeadline(time.Time{})
	}
	var resp response
	err = json.NewEncoder(c.conn).Encode(request{ID: c.next, Op: op, Args: raw})
	if err == nil {
		err = c.dec.Decode(&resp)
	}
	if err == nil && resp.ID != c.next {
		err = fmt.Errorf("response %d to request %d", resp.ID, c.next)
	}
	if err != nil {
		c.conn.Close()
		c.conn = nil
		return fmt.Errorf("wire: %s: %w", op, err)
	}
	if resp.Error != nil {
		return decodeError(resp.Error)
	}
	if result != nil && resp.Result != nil {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

func (c *Client) Threads(ctx context.Context, f control.Filter) ([]control.ThreadInfo, error) {
	var out []control.ThreadInfo
	err := c.call(ctx, "Threads", threadsArgs{Filter: f}, &out)
	return out, err
}

func (c *Client) Thread(ctx context.Context, id control.ThreadID, recent int) (control.ThreadDetail, error) {
	var out control.ThreadDetail
	err := c.call(ctx, "Thread", threadArgs{ID: id, Recent: recent}, &out)
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

// Events follows the daemon's events on a connection of its own. When the
// connection breaks, it connects again and resumes after the last event it
// delivered, so a reader sees no gap and no repeat. The first request is
// made before Events returns, so a refusal is returned as an error.
func (c *Client) Events(ctx context.Context, f control.Filter, after uint64) (<-chan control.Event, error) {
	conn, dec, err := c.openStream(f, after)
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
				if conn, dec, err = c.openStream(f, after); err == nil {
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

func (c *Client) openStream(f control.Filter, after uint64) (net.Conn, *json.Decoder, error) {
	conn, dec, err := c.connect()
	if err != nil {
		return nil, nil, err
	}
	raw, _ := json.Marshal(eventsArgs{Filter: f, After: after})
	var resp response
	err = json.NewEncoder(conn).Encode(request{ID: 1, Op: "Events", Args: raw})
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
		if resp.Event.Pos <= after {
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
