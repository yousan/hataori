package machineio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

var ErrAttachmentLost = errors.New("worker attachment connection ended; keys were not replayed")

type Attachment struct {
	conn          net.Conn
	reader        *bufio.Reader
	protocol      *workerproto.Connection
	remote        *Remote
	request       workerproto.Message
	handle        string
	cols, rows    int
	order, offset uint64
	mu            sync.Mutex
	pending       map[string]string
	closed        bool
}

func (r *Remote) Attach(ctx context.Context) (*Attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, p, err := r.Open(ctx)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			c.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	m, err := r.Request("attach", "", workerproto.Attachment{Handle: operationID(), Cols: 80, Rows: 24})
	if err != nil {
		return nil, err
	}
	b, err := workerproto.Encode(m)
	if err != nil {
		return nil, err
	}
	required, _ := r.Request("describe", "", workerproto.Empty{})
	requiredData, _ := workerproto.Encode(required)
	if _, err := p.Decode(requiredData, &r.Target); err != nil {
		return nil, err
	}
	if _, err := p.Decode(b, &r.Target); err != nil {
		return nil, err
	}
	if n, err := c.Write(b); err != nil {
		return nil, err
	} else if n != len(b) {
		return nil, io.ErrShortWrite
	}
	reader := bufio.NewReaderSize(c, workerproto.MaxFrame)
	data, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	reply, err := p.Decode(data, &r.Target)
	if err != nil {
		return nil, err
	}
	if reply.ID != m.ID || reply.Op != "attach" || reply.Kind != "reply" {
		return nil, errors.New("worker attachment was refused")
	}
	var opened workerproto.Attachment
	if err := json.Unmarshal(reply.Body, &opened); err != nil {
		return nil, err
	}
	if !stop() {
		return nil, ctx.Err()
	}
	c.SetDeadline(time.Time{})
	success = true
	return &Attachment{conn: c, reader: reader, protocol: p, remote: r, request: m, handle: opened.Handle, cols: int(opened.Cols), rows: int(opened.Rows), pending: map[string]string{}}, nil
}

func (a *Attachment) Next() (host.TerminalChunk, error) {
	for {
		data, err := a.reader.ReadSlice('\n')
		if err != nil {
			return host.TerminalChunk{}, fmt.Errorf("%w: %v", ErrAttachmentLost, err)
		}
		m, err := a.protocol.Decode(data, &a.remote.Target)
		if err != nil {
			return host.TerminalChunk{}, err
		}
		if m.Kind == "reply" {
			a.mu.Lock()
			op, ok := a.pending[m.ID]
			delete(a.pending, m.ID)
			a.mu.Unlock()
			if !ok || m.Op != op {
				return host.TerminalChunk{}, errors.New("worker attachment reply is not expected")
			}
			continue
		}
		if m.Kind != "stream" || m.Op != "attach" || m.ID != a.request.ID {
			return host.TerminalChunk{}, ErrAttachmentLost
		}
		var b workerproto.Bytes
		json.Unmarshal(m.Body, &b)
		if b.Handle != a.handle || b.Source.Source != "terminal" || b.Generation != a.remote.Target.Run || b.Order != a.order+1 || b.Offset != a.offset || b.State != "data" && b.State != "empty" {
			return host.TerminalChunk{}, errors.New("worker attachment stream identity or range changed")
		}
		a.order, a.offset = b.Order, b.Next
		if b.Ended {
			return host.TerminalChunk{}, io.EOF
		}
		return host.TerminalChunk{Run: a.remote.Target.Run, Pos: b.Next, Data: b.Data, Cols: int(b.Cols), Rows: int(b.Rows), Reset: b.Reset}, nil
	}
}

func (a *Attachment) send(op string, body any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return net.ErrClosed
	}
	if len(a.pending) >= 64 {
		return errors.New("worker attachment has too many unacknowledged key batches")
	}
	m, err := a.remote.Request(op, "", body)
	if err != nil {
		return err
	}
	b, err := workerproto.Encode(m)
	if err != nil {
		return err
	}
	a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	a.pending[m.ID] = op
	n, err := a.conn.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}
func (a *Attachment) Input(b []byte) error {
	return a.send("keys", workerproto.Keys{Handle: a.handle, Data: b})
}
func (a *Attachment) Resize(cols, rows int) error {
	if cols <= 0 || cols > 65535 || rows <= 0 || rows > 65535 {
		return errors.New("worker attachment: invalid size")
	}
	return a.send("resize", workerproto.Attachment{Handle: a.handle, Cols: uint16(cols), Rows: uint16(rows)})
}
func (a *Attachment) Size() (int, int) { return a.cols, a.rows }
func (a *Attachment) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return a.conn.Close()
}
