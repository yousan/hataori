package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// ErrNotSupported is the answer to a request whose operation the other
// side does not know, and the core's to an operation it does not answer
// (control.ErrNotSupported).
var ErrNotSupported = control.ErrNotSupported

// Handler answers the requests the other side of a connection sends.
// It returns ErrNotSupported for an operation it does not know.
type Handler func(ctx context.Context, op string, args json.RawMessage) (any, error)

// message is any line after the hellos: a request when Op is set, else the
// response to the request with the same ID sent by this side.
type message struct {
	ID     uint64          `json:"id"`
	Op     string          `json:"op,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
	Event  *control.Event  `json:"event,omitempty"`
}

// Peer is one side of a connection on which both sides send requests.
// Each side numbers its own requests; a response carries the number of the
// request it answers.
type Peer struct {
	w       io.Writer
	handler Handler

	wmu     sync.Mutex // one line at a time
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan message
	err     error // the connection broke
	done    chan struct{}
}

func newPeer(w io.Writer, h Handler) *Peer {
	return &Peer{w: w, handler: h, pending: map[uint64]chan message{}, done: make(chan struct{})}
}

func (p *Peer) write(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.w.Write(append(b, '\n'))
	return err
}

// writeCtx is write, given up when ctx ends: a side that stops reading
// cannot hold the writer forever.
func (p *Peer) writeCtx(ctx context.Context, m any) error {
	conn, ok := p.w.(net.Conn)
	if !ok {
		return p.write(m)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetWriteDeadline(time.Unix(1, 0)) })
	err := p.write(m)
	if !stop() {
		// ctx ended during the write: the line may be cut, so the
		// connection is of no further use.
		conn.Close()
		return ctx.Err()
	}
	return err
}

// Request sends op to the other side and waits for its answer, which is
// decoded into result when result is not nil.
func (p *Peer) Request(ctx context.Context, op string, args, result any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return p.err
	}
	p.next++
	id := p.next
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()
	if err := p.writeCtx(ctx, request{ID: id, Op: op, Args: raw}); err != nil {
		return fmt.Errorf("wire: %s: %w", op, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return decodeError(m.Error)
		}
		if result != nil && m.Result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-p.done:
		return fmt.Errorf("wire: %s: %w", op, p.err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done is closed when the connection breaks, or when it becomes a stream:
// from then on Request fails at once.
func (p *Peer) Done() <-chan struct{} { return p.done }

// serve reads the connection until it breaks. Requests are answered by the
// handler, each in its own goroutine, so that a handler may itself send a
// request and wait. stream, when not nil, takes over the connection for an
// Events request.
func (p *Peer) serve(ctx context.Context, dec *json.Decoder, stream func(request) bool) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var m message
		err := dec.Decode(&m)
		if err != nil {
			p.close(err)
			return err
		}
		if m.Op == "" {
			p.mu.Lock()
			ch := p.pending[m.ID]
			p.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		req := request{ID: m.ID, Op: m.Op, Args: m.Args}
		if stream != nil && stream(req) {
			p.close(io.EOF)
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result any
			err := ErrNotSupported
			if p.handler != nil {
				result, err = p.handler(ctx, req.Op, req.Args)
			}
			resp := response{ID: req.ID}
			if err != nil {
				resp.Error = encodeError(err)
			} else if result != nil {
				resp.Result, _ = json.Marshal(result)
			}
			p.write(resp)
		}()
	}
}

func (p *Peer) close(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		if err == nil || errors.Is(err, io.EOF) || strings.Contains(err.Error(), "use of closed") {
			err = errors.New("the connection closed")
		}
		p.err = err
		close(p.done)
	}
}

// major is the major number of a version such as "1.0".
func major(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}

// tokensSince is the first version whose daemon honours a hello's token.
// An older daemon ignores the field and makes the connection the owner's.
const tokensSince = "2.2"

// atLeast reports whether version v is min or later, by number. A version
// that is not two numbers is not.
func atLeast(v, min string) bool {
	parse := func(s string) (int, int, bool) {
		a, b, ok := strings.Cut(s, ".")
		x, err1 := strconv.Atoi(a)
		y, err2 := strconv.Atoi(b)
		return x, y, ok && err1 == nil && err2 == nil
	}
	x, y, ok := parse(v)
	mx, my, _ := parse(min)
	return ok && (x > mx || x == mx && y >= my)
}
