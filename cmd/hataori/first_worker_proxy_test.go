package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yousan/hataori/internal/workerproto"
)

// A fixture-only relay sits behind the actual SSH Unix forward. It can lose a
// completed reply or duplicate an identical operation without changing either
// product endpoint. Every pathname and connection belongs to this test.
type firstWorkerProxy struct {
	t            *testing.T
	listener     *net.UnixListener
	path, actual string
	info, real   os.FileInfo
	mu           sync.Mutex
	connections  map[net.Conn]bool
	wg           sync.WaitGroup
	away         atomic.Bool
	duplicate    atomic.Bool
	dropInput    atomic.Bool
	hideReceipt  atomic.Bool
	duplicates   atomic.Int64
	dropped      atomic.Int64
}

func newFirstWorkerProxy(t *testing.T, path string) *firstWorkerProxy {
	t.Helper()
	p := &firstWorkerProxy{t: t, path: path, actual: path + ".fixture", connections: map[net.Conn]bool{}}
	var err error
	p.real, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, p.actual); err != nil {
		t.Fatal(err)
	}
	p.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	p.listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	p.info, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := p.listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.away.Load() {
				p.mu.Unlock()
				c.Close()
				continue
			}
			p.connections[c] = true
			p.wg.Add(1)
			p.mu.Unlock()
			go p.serve(c)
		}
	}()
	return p
}

func (p *firstWorkerProxy) serve(client net.Conn) {
	defer p.wg.Done()
	defer func() {
		client.Close()
		p.mu.Lock()
		delete(p.connections, client)
		p.mu.Unlock()
	}()
	upstream, err := net.Dial("unix", p.actual)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.away.Load() {
		p.mu.Unlock()
		return
	}
	p.connections[upstream] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, upstream); p.mu.Unlock() }()
	requests, replies := bufio.NewReader(client), bufio.NewReader(upstream)
	for {
		line, err := requests.ReadBytes('\n')
		if err != nil {
			return
		}
		var request workerproto.Message
		json.Unmarshal(line, &request)
		duplicate := request.Op == "input" && p.duplicate.CompareAndSwap(true, false)
		if _, err := io.Copy(upstream, bytes.NewReader(line)); err != nil {
			return
		}
		if duplicate {
			if _, err := io.Copy(upstream, bytes.NewReader(line)); err != nil {
				return
			}
			p.duplicates.Add(1)
		}
		answer, err := replies.ReadBytes('\n')
		if err != nil {
			return
		}
		if duplicate {
			if _, err := replies.ReadBytes('\n'); err != nil {
				return
			}
		}
		var reply workerproto.Message
		json.Unmarshal(answer, &reply)
		var receipt workerproto.Receipt
		if request.Op == "input" && reply.Kind == "reply" && json.Unmarshal(reply.Body, &receipt) == nil && receipt.State == "complete" && p.dropInput.CompareAndSwap(true, false) {
			p.dropped.Add(1)
			return // The worker applied it; the main receives no acknowledgement.
		}
		if request.Op == "receipt" && p.hideReceipt.Load() {
			body, _ := json.Marshal(workerproto.Refusal{Code: "not_found", Message: "isolated temporarily hidden receipt"})
			reply.Kind, reply.Body = "refusal", body
			answer, err = workerproto.Encode(reply)
			if err != nil {
				return
			}
		}
		if _, err := io.Copy(client, bytes.NewReader(answer)); err != nil {
			return
		}
		if request.Op == "attach" && reply.Kind == "reply" {
			// Attachment frames are a duplex stream, not request/reply RPCs.
			done := make(chan struct{})
			go func() { io.Copy(upstream, requests); upstream.Close(); close(done) }()
			io.Copy(client, replies)
			client.Close()
			<-done
			return
		}
	}
}

func (p *firstWorkerProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.away.Store(true)
	for c := range p.connections {
		c.Close()
	}
}

func (p *firstWorkerProxy) close() {
	p.cut()
	p.listener.Close()
	p.wg.Wait()
	if info, err := os.Lstat(p.path); err == nil && os.SameFile(info, p.info) {
		os.Remove(p.path)
	}
}

func (p *firstWorkerProxy) removeActual() {
	if info, err := os.Lstat(p.actual); err == nil && os.SameFile(info, p.real) {
		os.Remove(p.actual)
	}
}
