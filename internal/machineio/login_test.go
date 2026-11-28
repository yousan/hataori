package machineio

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

func loginWireFixture(t *testing.T) (*Login, net.Conn) {
	t.Helper()
	binding := workerproto.Binding{Main: "main", Worker: "worker"}
	h := workerproto.Hello{Version: workerproto.Version, Binding: binding, Capabilities: []string{"login"}}
	p, err := workerproto.Negotiate(binding, h, h)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	l := &Login{conn: client, reader: bufio.NewReaderSize(client, workerproto.MaxFrame), protocol: p, request: workerproto.Message{Kind: "request", ID: "start", Binding: binding, Op: "login_start"}, attempt: "attempt", pending: map[string]bool{}, cancelAck: make(chan struct{}), stop: func() bool { return false }}
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
	frame := loginWireMessage(t, l, "stream", "login_start", "start", workerproto.LoginStream{Attempt: "attempt", State: "complete"})
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
				frame = loginWireMessage(t, l, "stream", "login_start", "start", workerproto.LoginStream{Attempt: "another", State: "data", Data: []byte(`{"link":"https://example.invalid"}`)})
			case "input-ack-attempt":
				l.pending["input"] = true
				frame = loginWireMessage(t, l, "reply", "login_input", "input", workerproto.LoginAttempt{Attempt: "another"})
			case "cancel-ack-id":
				l.cancelID = "cancel"
				frame = loginWireMessage(t, l, "reply", "login_cancel", "another", workerproto.LoginAttempt{Attempt: "attempt"})
			}
			var following []byte
			if kind == "input-ack-attempt" {
				following = loginWireMessage(t, l, "stream", "login_start", "start", workerproto.LoginStream{Attempt: "attempt", State: "data", Data: []byte(`{"no_code":true}`)})
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
