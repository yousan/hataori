package machineio

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

type loginWriteObservation struct {
	net.Conn
	writes atomic.Int32
}

func (c *loginWriteObservation) Write([]byte) (int, error) {
	c.writes.Add(1)
	return 0, io.ErrClosedPipe
}

func TestWorkerLoginContractRequiredBeforeProbe(t *testing.T) {
	for _, version := range []string{"1.3", "1.4"} {
		t.Run(version, func(t *testing.T) {
			binding := workerproto.Binding{Main: "main", Worker: "worker"}
			local := workerproto.Hello{Version: workerproto.Version, Binding: binding, Capabilities: []string{"health", "login", workerproto.LoginCapability}}
			peer := local
			peer.Version = version
			if version == "1.4" {
				peer.Capabilities = []string{"health", "login"}
			}
			p, err := workerproto.Negotiate(binding, local, peer)
			if err != nil {
				t.Fatal(err)
			}
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			observed := &loginWriteObservation{Conn: a}
			open := func(context.Context) (net.Conn, *workerproto.Connection, error) { return observed, p, nil }
			l, err := StartLogin(context.Background(), binding, open, workerproto.AgentConfig{Agent: "fake", Config: "default"}, "")
			if l != nil || err == nil || observed.writes.Load() != 0 {
				t.Fatal("unsupported login contract reached worker probe")
			}
		})
	}
}

func loginWireFixture(t *testing.T) (*Login, net.Conn) {
	t.Helper()
	binding := workerproto.Binding{Main: "main", Worker: "worker"}
	h := workerproto.Hello{Version: workerproto.Version, Binding: binding, Capabilities: []string{workerproto.LoginCapability}}
	p, err := workerproto.Negotiate(binding, h, h)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	l := &Login{conn: client, reader: bufio.NewReaderSize(client, workerproto.MaxFrame), protocol: p, request: workerproto.Message{Kind: "request", ID: "start", Binding: binding, Op: "ephemeral_login_start"}, attempt: "attempt", pending: map[string]bool{}, cancelAck: make(chan struct{}), stop: func() bool { return false }}
	return l, server
}

func loginWireMessage(t *testing.T, l *Login, kind, op, id string, body any) []byte {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := workerproto.Encode(workerproto.Message{Kind: kind, ID: id, Binding: l.request.Binding, Op: op, Body: b})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestWorkerLoginCompleteWakesConcurrentCancel(t *testing.T) {
	l, server := loginWireFixture(t)
	frame := loginWireMessage(t, l, "stream", "ephemeral_login_start", "start", workerproto.LoginStream{Attempt: "attempt", State: "complete"})
	readDone := make(chan error, 1)
	go func() { _, err := l.Next(); readDone <- err }()
	go func() {
		reader := bufio.NewReader(server)
		if _, err := reader.ReadBytes('\n'); err != nil {
			return
		} // cancel is definitely in flight
		server.Write(frame) // completion is the cleanup barrier; no cancel reply follows
		server.Close()
	}()
	if err := l.Cancel(); err != nil {
		t.Fatal("completed cleanup did not wake concurrent cancellation")
	}
	if err := <-readDone; err != io.EOF {
		t.Fatal("completion did not end the read")
	}
}

func TestWorkerLoginWireCorrelations(t *testing.T) {
	for _, kind := range []string{"stream-attempt", "input-ack-attempt", "cancel-ack-id"} {
		t.Run(kind, func(t *testing.T) {
			l, server := loginWireFixture(t)
			var frame []byte
			switch kind {
			case "stream-attempt":
				frame = loginWireMessage(t, l, "stream", "ephemeral_login_start", "start", workerproto.LoginStream{Attempt: "another", State: "data", Data: []byte(`{"link":"https://example.invalid"}`)})
			case "input-ack-attempt":
				l.pending["input"] = true
				frame = loginWireMessage(t, l, "reply", "ephemeral_login_input", "input", workerproto.LoginAttempt{Attempt: "another"})
			case "cancel-ack-id":
				l.cancelID = "cancel"
				frame = loginWireMessage(t, l, "reply", "ephemeral_login_cancel", "another", workerproto.LoginAttempt{Attempt: "attempt"})
			}
			var following []byte
			if kind == "input-ack-attempt" {
				following = loginWireMessage(t, l, "stream", "ephemeral_login_start", "start", workerproto.LoginStream{Attempt: "attempt", State: "data", Data: []byte(`{"no_code":true,"show_code":"fixture-code"}`)})
			}
			go func() {
				server.Write(frame)
				if len(following) > 0 {
					server.Write(following)
				}
				server.Close()
			}()
			l.conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := l.Next(); err == nil || err == io.EOF {
				t.Fatal("uncorrelated login frame was accepted")
			}
			l.mu.Lock()
			ended := l.ended
			l.mu.Unlock()
			if ended {
				t.Fatal("uncorrelated login frame was accepted")
			}
		})
	}
}
