package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// operateSince is the first version whose daemon knows OperateHost.
const operateSince = "3.11"

// operate serves an OperateHost request: it starts the operation, answers
// with its idle period, then carries out the client's HostKeys and End
// requests in order, answering each, until the operation ends; its last
// message names the reason. A connection that closes ends the operation.
func operate(ctx context.Context, api control.API, c net.Conn, dec *json.Decoder, enc *json.Encoder, req request) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var a operateHostArgs
	if err := json.Unmarshal(req.Args, &a); err != nil {
		enc.Encode(response{ID: req.ID, Error: encodeError(fmt.Errorf("wire: OperateHost: bad arguments: %w", err))})
		return
	}
	op, err := api.OperateHost(ctx, a.Pane)
	if err != nil {
		enc.Encode(response{ID: req.ID, Error: encodeError(err)})
		return
	}
	var wmu sync.Mutex
	send := func(r response) error {
		wmu.Lock()
		defer wmu.Unlock()
		return enc.Encode(r)
	}
	result, _ := json.Marshal(hostOperation{Idle: int64(op.Idle() / time.Second)})
	if err := send(response{ID: req.ID, Result: result}); err != nil {
		return
	}
	go func() {
		defer cancel() // the client went away: the operation ends
		for {
			var m message
			if err := dec.Decode(&m); err != nil {
				return
			}
			if m.Op == "" {
				continue // a response: the daemon sends no request here
			}
			var err error
			switch m.Op {
			case "HostKeys":
				var k hostKeysArgs
				if err = json.Unmarshal(m.Args, &k); err == nil {
					err = op.Send(ctx, k.Keys)
				} else {
					// The message of a decoding error may quote the text.
					err = errors.New("wire: HostKeys: bad arguments")
				}
			case "End":
				op.End()
			default:
				err = fmt.Errorf("wire: operation %q on an OperateHost stream: %w", m.Op, ErrNotSupported)
			}
			resp := response{ID: m.ID}
			if err != nil {
				resp.Error = encodeError(err)
			}
			if send(resp) != nil {
				return
			}
		}
	}()
	<-op.Done()
	send(response{ID: req.ID, Ended: op.Reason()})
}

// OperateHost starts typing into a tmux pane on a connection of its own.
// The operation ends as control.HostOperation says; when the connection
// breaks without the daemon's last message, its Reason is "disconnected".
// It needs a daemon of 3.11 or later.
func (c *Client) OperateHost(ctx context.Context, pane string) (control.HostOperation, error) {
	conn, dec, version, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	if !atLeast(version, operateSince) {
		conn.Close()
		return nil, fmt.Errorf("wire: the daemon speaks %s; typing into a host pane needs %s or later: upgrade the daemon", version, operateSince)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	raw, _ := json.Marshal(operateHostArgs{Pane: pane})
	var resp response
	err = json.NewEncoder(conn).Encode(request{ID: 1, Op: "OperateHost", Args: raw})
	if err == nil {
		err = dec.Decode(&resp)
	}
	if err == nil && resp.Error != nil {
		err = decodeError(resp.Error)
	}
	var info hostOperation
	if err == nil {
		err = json.Unmarshal(resp.Result, &info)
	}
	if err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	o := &clientOp{conn: conn, enc: json.NewEncoder(conn), idle: time.Duration(info.Idle) * time.Second,
		next: 1, pending: map[uint64]chan response{}, done: make(chan struct{})}
	go o.read(dec, stop)
	return o, nil
}

// clientOp is an operation on the client's side of an OperateHost stream.
type clientOp struct {
	conn net.Conn
	enc  *json.Encoder
	idle time.Duration

	wmu     sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan response
	reason  string
	done    chan struct{}
}

// read delivers the daemon's answers until its last message or until the
// connection breaks.
func (o *clientOp) read(dec *json.Decoder, stop func() bool) {
	reason := "disconnected"
	for {
		var resp response
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.ID == 1 {
			if resp.Ended != "" {
				reason = resp.Ended
				break
			}
			continue
		}
		o.mu.Lock()
		ch := o.pending[resp.ID]
		delete(o.pending, resp.ID)
		o.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	stop()
	o.conn.Close()
	o.mu.Lock()
	o.reason = reason
	close(o.done)
	o.mu.Unlock()
}

// request sends op and waits for its answer.
func (o *clientOp) request(ctx context.Context, op string, args any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ch := make(chan response, 1)
	o.mu.Lock()
	select {
	case <-o.done:
		o.mu.Unlock()
		return fmt.Errorf("wire: %s: %w", op, control.ErrEnded)
	default:
	}
	o.next++
	id := o.next
	o.pending[id] = ch
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.pending, id)
		o.mu.Unlock()
	}()
	o.wmu.Lock()
	err = o.enc.Encode(request{ID: id, Op: op, Args: raw})
	o.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("wire: %s: %w", op, control.ErrEnded)
	}
	select {
	case resp := <-ch:
		return decodeError(resp.Error)
	case <-o.done:
		return fmt.Errorf("wire: %s: %w", op, control.ErrEnded)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *clientOp) Send(ctx context.Context, keys []control.HostKey) error {
	return o.request(ctx, "HostKeys", hostKeysArgs{Keys: keys})
}

func (o *clientOp) Idle() time.Duration   { return o.idle }
func (o *clientOp) Done() <-chan struct{} { return o.done }

// End asks the daemon to end the operation; Done is closed when it has.
// When the daemon cannot be told, the connection is closed, which ends it
// too.
func (o *clientOp) End() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.request(ctx, "End", endArgs{}); err != nil && !errors.Is(err, control.ErrEnded) {
		o.conn.Close()
	}
}

func (o *clientOp) Reason() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reason
}
