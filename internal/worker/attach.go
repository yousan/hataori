package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

func (r *Resident) openAttachment(ctx context.Context, p *workerproto.Connection, data []byte) (workerproto.Message, *host.Attachment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var route workerproto.Message
	json.Unmarshal(data, &route)
	reply := workerproto.Message{Kind: "refusal", ID: route.ID, Binding: r.binding, Target: route.Target, Op: "attach"}
	deny := func(code, message string) (workerproto.Message, *host.Attachment) {
		reply.Body, _ = json.Marshal(failure(code, message))
		return reply, nil
	}
	if r.closed || !r.ready || r.visibleRoot() != nil {
		return deny("unavailable", "resident is not ready")
	}
	if route.Target == nil {
		return deny("wrong_target", "attachment must name a run")
	}
	run := r.state.Runs[targetKey(*route.Target)]
	if run == nil || r.checkMarker(run) != nil {
		return deny("wrong_target", "attachment does not name an owned run")
	}
	m, err := p.Decode(data, &run.Target)
	if err != nil || m.Kind != "request" || m.Op != "attach" {
		return deny("invalid_message", "attachment request is invalid")
	}
	if !run.Terminal || run.State != "running" {
		return deny("not_ready", "owned run has no running terminal")
	}
	a, err := host.AttachIdentity(ctx, r.hostDir(run.Target), run.Identity)
	if err != nil {
		return deny("unavailable", "owned host does not answer")
	}
	cols, rows := a.Size()
	reply.Kind = "reply"
	reply.Body, _ = json.Marshal(workerproto.Attachment{Handle: newID(), Cols: uint16(cols), Rows: uint16(rows)})
	return reply, a
}

// One connection owns one attachment handle. A read error closes both sides;
// no handle, input batch or terminal key survives this connection.
func (r *Resident) serveAttachment(ctx context.Context, c net.Conn, reader *bufio.Reader, p *workerproto.Connection, data []byte) {
	reply, a := r.openAttachment(ctx, p, data)
	if sendFrame(c, reply) != nil || a == nil {
		if a != nil {
			a.Close()
		}
		return
	}
	defer a.Close()
	var opened workerproto.Attachment
	json.Unmarshal(reply.Body, &opened)
	var sendMu sync.Mutex
	send := func(m workerproto.Message) error { sendMu.Lock(); defer sendMu.Unlock(); return sendFrame(c, m) }
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	go func() {
		defer c.Close()
		var order, position uint64
		for {
			ch, err := a.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					order++
					m := reply
					m.Kind = "stream"
					m.Body, _ = json.Marshal(workerproto.Bytes{Source: workerproto.Source{Source: "terminal", Handle: opened.Handle, Generation: reply.Target.Run}, Order: order, Offset: position, Next: position, State: "empty", Ended: true})
					send(m)
				}
				return
			}
			for first := true; first || len(ch.Data) > 0; first = false {
				n := min(len(ch.Data), workerproto.MaxChunk)
				b := ch.Data[:n]
				ch.Data = ch.Data[n:]
				order++
				out := workerproto.Bytes{Source: workerproto.Source{Source: "terminal", Handle: opened.Handle, Generation: reply.Target.Run}, Order: order, Offset: position, Next: position + uint64(n), State: "empty", Data: b, Cols: uint16(ch.Cols), Rows: uint16(ch.Rows), Reset: ch.Reset && first}
				if n > 0 {
					out.State = "data"
				}
				position = out.Next
				m := reply
				m.Kind = "stream"
				m.Body, _ = json.Marshal(out)
				if send(m) != nil {
					return
				}
			}
		}
	}()
	for {
		c.SetReadDeadline(time.Time{})
		data, err := readFrame(reader)
		if err != nil {
			return
		}
		m, err := p.Decode(data, reply.Target)
		if err != nil || m.Kind != "request" {
			return
		}
		var handle string
		switch m.Op {
		case "keys":
			var b workerproto.Keys
			json.Unmarshal(m.Body, &b)
			handle = b.Handle
			if handle != opened.Handle {
				return
			}
			err = a.Input(b.Data)
		case "resize":
			var b workerproto.Attachment
			json.Unmarshal(m.Body, &b)
			handle = b.Handle
			if handle != opened.Handle {
				return
			}
			err = a.Resize(int(b.Cols), int(b.Rows))
		case "detach":
			var b workerproto.Handle
			json.Unmarshal(m.Body, &b)
			if b.Handle != opened.Handle {
				return
			}
			return
		default:
			return
		}
		if err != nil {
			return
		}
		m.Kind = "reply"
		m.Body = json.RawMessage(`{}`)
		if send(m) != nil {
			return
		}
	}
}
