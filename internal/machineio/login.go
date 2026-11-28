package machineio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

var ErrLoginLost = errors.New("worker login ended; login material was not replayed")

// Login is an ephemeral conversation. It never uses Call, a receipt or a
// reconnect callback. The parent context must cover the whole attempt.
type Login struct {
	conn       net.Conn
	reader     *bufio.Reader
	protocol   *workerproto.Connection
	request    workerproto.Message
	attempt    string
	stop       func() bool
	mu         sync.Mutex
	pending    map[string]bool
	closed     bool
	ended      bool
	cancelID   string
	cancelAck  chan struct{}
	cleanupAck sync.Once
}

// StartLogin opens exactly once against main-owned binding/configuration.
func StartLogin(ctx context.Context, binding workerproto.Binding, open func(context.Context) (net.Conn, *workerproto.Connection, error), key workerproto.AgentConfig, expectedDir string) (*Login, error) {
	c, p, err := open(ctx)
	if err != nil {
		return nil, ErrLoginLost
	}
	good := false
	defer func() {
		if !good {
			c.Close()
		}
	}()
	if p == nil || p.Hello().Binding != binding || !p.CanLogin() {
		return nil, ErrLoginLost
	}
	l := &Login{conn: c, reader: bufio.NewReaderSize(c, workerproto.MaxFrame), protocol: p, attempt: operationID(), pending: map[string]bool{}, cancelAck: make(chan struct{})}
	l.stop = context.AfterFunc(ctx, func() { c.Close() })
	defer func() {
		if !good {
			l.stop()
		}
	}()
	// Verify the directory on this same bound connection. The resident's
	// registered configurations are immutable for its lifetime. This path is
	// compared as metadata; it is never a main-selected worker resource.
	probeBody, _ := json.Marshal(key)
	probe := workerproto.Message{Kind: "request", ID: operationID(), Binding: binding, Op: "probe", Body: probeBody}
	probeBytes, err := workerproto.Encode(probe)
	if err != nil {
		return nil, ErrLoginLost
	}
	if _, err := p.Decode(probeBytes, nil); err != nil {
		return nil, ErrLoginLost
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Write(probeBytes); err != nil || n != len(probeBytes) {
		return nil, ErrLoginLost
	}
	probeData, err := l.reader.ReadSlice('\n')
	if err != nil {
		return nil, ErrLoginLost
	}
	probeReply, err := p.Decode(probeData, nil)
	if err != nil || probeReply.Kind != "reply" || probeReply.Op != probe.Op || probeReply.ID != probe.ID {
		return nil, ErrLoginLost
	}
	var health workerproto.AgentHealth
	if json.Unmarshal(probeReply.Body, &health) != nil || health.AgentConfig != key || health.ConfigDir != expectedDir {
		return nil, ErrLoginLost
	}
	body, _ := json.Marshal(workerproto.Login{AgentConfig: key, Attempt: l.attempt})
	l.request = workerproto.Message{Kind: "request", ID: operationID(), Binding: binding, Op: "ephemeral_login_start", Body: body}
	b, err := workerproto.Encode(l.request)
	if err != nil {
		return nil, ErrLoginLost
	}
	if _, err := p.Decode(b, nil); err != nil {
		return nil, ErrLoginLost
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Write(b); err != nil || n != len(b) {
		return nil, ErrLoginLost
	}
	data, err := l.reader.ReadSlice('\n')
	if err != nil {
		return nil, ErrLoginLost
	}
	m, err := p.Decode(data, nil)
	if err != nil || m.Kind != "reply" || m.ID != l.request.ID || m.Op != "ephemeral_login_start" {
		return nil, ErrLoginLost
	}
	var started workerproto.LoginAttempt
	json.Unmarshal(m.Body, &started)
	if started.Attempt != l.attempt || ctx.Err() != nil {
		return nil, ErrLoginLost
	}
	c.SetDeadline(time.Time{})
	good = true
	return l, nil
}

// Next is read by one goroutine. No raw peer error or payload is exposed as
// an error that a caller might put in a diagnostic log.
func (l *Login) Next() (workerproto.LoginData, error) {
	for {
		b, err := l.reader.ReadSlice('\n')
		if err != nil {
			return workerproto.LoginData{}, ErrLoginLost
		}
		m, err := l.protocol.Decode(b, nil)
		if err != nil {
			return workerproto.LoginData{}, ErrLoginLost
		}
		if m.Kind == "reply" && m.Op == "ephemeral_login_input" {
			l.mu.Lock()
			ok := l.pending[m.ID]
			delete(l.pending, m.ID)
			l.mu.Unlock()
			var ack workerproto.LoginAttempt
			json.Unmarshal(m.Body, &ack)
			if !ok || ack.Attempt != l.attempt {
				return workerproto.LoginData{}, ErrLoginLost
			}
			continue
		}
		if m.Kind == "reply" && m.Op == "ephemeral_login_cancel" {
			l.mu.Lock()
			var ack workerproto.LoginAttempt
			json.Unmarshal(m.Body, &ack)
			if l.cancelID == "" || m.ID != l.cancelID || ack.Attempt != l.attempt {
				l.mu.Unlock()
				return workerproto.LoginData{}, ErrLoginLost
			}
			l.ended = true
			l.cleanupAck.Do(func() { close(l.cancelAck) })
			l.mu.Unlock()
			return workerproto.LoginData{}, ErrLoginLost
		}
		if m.Kind != "stream" || m.Op != "ephemeral_login_start" || m.ID != l.request.ID {
			return workerproto.LoginData{}, ErrLoginLost
		}
		var stream workerproto.LoginStream
		json.Unmarshal(m.Body, &stream)
		if stream.Attempt != l.attempt {
			return workerproto.LoginData{}, ErrLoginLost
		}
		if stream.State != "data" {
			if stream.State == "complete" {
				l.mu.Lock()
				l.ended = true
				l.cleanupAck.Do(func() { close(l.cancelAck) })
				l.mu.Unlock()
				return workerproto.LoginData{}, io.EOF
			}
			return workerproto.LoginData{}, ErrLoginLost
		}
		d, err := workerproto.DecodeLoginData(stream.Data)
		if err != nil {
			return workerproto.LoginData{}, ErrLoginLost
		}
		return d, nil
	}
}

// Cancel requires a concurrently running Next reader. Its correlated reply is
// a cleanup barrier; a lost reply never authorizes an automatic replacement.
func (l *Login) Cancel() error {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return nil
	}
	if l.closed || l.cancelID != "" {
		l.mu.Unlock()
		return ErrLoginLost
	}
	l.cancelID = operationID()
	body, _ := json.Marshal(workerproto.LoginAttempt{Attempt: l.attempt})
	m := workerproto.Message{Kind: "request", ID: l.cancelID, Binding: l.request.Binding, Op: "ephemeral_login_cancel", Body: body}
	if b, err := workerproto.Encode(m); err == nil {
		l.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		l.conn.Write(b) // partial/failed writes are never retried
	}
	l.mu.Unlock()
	// Even a failed cancel write may race a valid complete frame already in
	// flight. Wait only for the same verified cleanup barrier, without retry.
	select {
	case <-l.cancelAck:
		return nil
	case <-time.After(2 * time.Second):
		return ErrLoginLost
	}
}

func (l *Login) Code(code string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || len(code) == 0 || len(code) > 16384 || len(l.pending) >= 16 {
		return ErrLoginLost
	}
	body, _ := json.Marshal(workerproto.LoginInput{Attempt: l.attempt, Data: []byte(code)})
	m := workerproto.Message{Kind: "request", ID: operationID(), Binding: l.request.Binding, Op: "ephemeral_login_input", Body: body}
	b, err := workerproto.Encode(m)
	if err != nil {
		return ErrLoginLost
	}
	l.pending[m.ID] = true
	l.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if n, err := l.conn.Write(b); err != nil || n != len(b) {
		l.closed = true
		l.conn.Close()
		return ErrLoginLost
	}
	return nil
}

func (l *Login) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.stop()
	return l.conn.Close()
}
