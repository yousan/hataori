package host

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type inputWriterFunc func([]byte) (int, error)

func (f inputWriterFunc) Write(b []byte) (int, error) { return f(b) }

func TestInputReplyPrecedesShutdown(t *testing.T) {
	h := &host{done: make(chan struct{})}
	h.input = inputWriterFunc(func(b []byte) (int, error) {
		close(h.done) // the final input lets the command exit
		return len(b), nil
	})
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	encoding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	enc := json.NewEncoder(inputWriterFunc(func(b []byte) (int, error) {
		close(encoding)
		<-release // descheduled after delivering input, before its answer
		return server.Write(b)
	}))
	replied := make(chan error, 1)
	go func() { replied <- h.reply(server, enc, request{Op: "input", Data: []byte(pasteEnd)}) }()
	select {
	case <-encoding:
	case <-time.After(10 * time.Second):
		t.Fatal("input did not reach its reply")
	}
	answer := make(chan response, 1)
	go func() {
		var resp response
		if err := json.NewDecoder(client).Decode(&resp); err != nil {
			resp.Error = err.Error()
		}
		answer <- resp
	}()
	ended := make(chan struct{})
	go func() {
		h.finishRequests(inputReplyTimeout)
		server.Close() // returning from Run ends the process and its sockets
		close(ended)
	}()
	waitFor(t, "shutdown to close registration", func() bool {
		h.stopMu.Lock()
		defer h.stopMu.Unlock()
		return h.closing
	})
	select {
	case <-ended:
		t.Fatal("host ended before the accepted input's reply")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case resp := <-answer:
		if !resp.OK || resp.Error != "" {
			t.Fatalf("input answer: %+v", resp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("input answer did not arrive")
	}
	if err := <-replied; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("host did not finish after replying")
	}
}

type blockedInput struct {
	*io.PipeWriter
	entered chan struct{}
}

func (w blockedInput) Write(b []byte) (int, error) {
	close(w.entered)
	return w.PipeWriter.Write(b)
}

func TestShutdownUnblocksInputBeforeWaitingForItsReply(t *testing.T) {
	r, w := io.Pipe() // an input reader retained without reading
	defer r.Close()
	defer w.Close()
	entered := make(chan struct{})
	h := &host{done: make(chan struct{}), input: blockedInput{w, entered}}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	replied := make(chan error, 1)
	go func() {
		replied <- h.reply(server, json.NewEncoder(server), request{Op: "input", Data: []byte("input")})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("input did not start")
	}
	close(h.done)
	ended := make(chan struct{})
	go func() { h.finishRequests(inputReplyTimeout); close(ended) }()
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	var resp response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == "" {
		t.Fatalf("blocked input answer: %+v", resp)
	}
	if err := <-replied; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("host did not finish after unblocking input")
	}
}

func TestShutdownBoundsAnUninterruptibleInput(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, HostLog))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := &host{done: make(chan struct{}), log: &hostLog{f: f}}
	h.input = inputWriterFunc(func(b []byte) (int, error) {
		close(entered)
		<-release // a blocking terminal write that Close cannot interrupt
		return len(b), nil
	})
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	replied := make(chan struct{})
	defer func() {
		close(release)
		client.Close()
		select {
		case <-replied:
		case <-time.After(10 * time.Second):
			t.Error("input did not finish after release")
		}
	}()
	go func() {
		h.reply(server, json.NewEncoder(server), request{Op: "input", Data: []byte("input")})
		close(replied)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("input did not start")
	}
	close(h.done)
	ended := make(chan struct{})
	go func() { h.finishRequests(50 * time.Millisecond); close(ended) }()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("uninterruptible input prevented shutdown")
	}
	if got := read(t, dir, HostLog); !bytes.Contains(got, []byte("input replies did not finish within 50ms")) {
		t.Fatalf("missing input timeout diagnosis: %s", got)
	}
}
