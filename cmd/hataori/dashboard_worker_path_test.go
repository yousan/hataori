package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/workerproto"
)

// dashboardWorkerTrace forwards unchanged bytes to the disposable resident.
// It retains only request operation/target headers, never payloads or keys.
// This observes the real main/SSH/worker path, with the existing fake agent;
// it is not a provider CLI capture or proof of dashboard owner attachment.
type dashboardWorkerTrace struct {
	listener *net.UnixListener
	path     string
	info     os.FileInfo
	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]bool
	requests []dashboardWorkerRequest
	wg       sync.WaitGroup
}

type dashboardWorkerRequest struct {
	Op     string
	Target *workerproto.Target
}

func traceDashboardWorker(t *testing.T, path, upstream string) *dashboardWorkerTrace {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	p := &dashboardWorkerTrace{listener: l, path: path, info: info, conns: map[net.Conn]bool{}}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				c.Close()
				continue
			}
			p.conns[c] = true
			p.wg.Add(1)
			p.mu.Unlock()
			go p.forward(c, upstream)
		}
	}()
	return p
}

func (p *dashboardWorkerTrace) forward(c net.Conn, upstream string) {
	defer p.wg.Done()
	defer func() { c.Close(); p.mu.Lock(); delete(p.conns, c); p.mu.Unlock() }()
	u, err := net.Dial("unix", upstream)
	if err != nil {
		return
	}
	defer u.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.conns[u] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.conns, u); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer u.Close()
		reader := bufio.NewReaderSize(c, workerproto.MaxFrame)
		for {
			frame, err := reader.ReadSlice('\n')
			if err != nil {
				return
			}
			var m workerproto.Message
			if json.Unmarshal(frame, &m) == nil && m.Kind == "request" {
				p.mu.Lock()
				p.requests = append(p.requests, dashboardWorkerRequest{m.Op, m.Target})
				p.mu.Unlock()
			}
			if _, err := io.Copy(u, bytes.NewReader(frame)); err != nil {
				return
			}
		}
	}()
	io.Copy(c, u)
	c.Close()
	<-done
}

func (p *dashboardWorkerTrace) snapshot() []dashboardWorkerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]dashboardWorkerRequest(nil), p.requests...)
}

func (p *dashboardWorkerTrace) close() {
	p.mu.Lock()
	p.closed = true
	p.listener.Close()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	if info, err := os.Lstat(p.path); err == nil && os.SameFile(info, p.info) {
		os.Remove(p.path)
	}
}

func TestDashboardRecordedWorkerPassiveTerminal(t *testing.T) {
	f := newFirstWorker(t)
	// Stop only the fixture-owned main and its established SSH descendants
	// before replacing its fault relay with this observation-only relay.
	f.kill()
	f.proxy.close()
	trace := traceDashboardWorker(t, f.proxy.path, f.proxy.actual)
	t.Cleanup(func() { defer trace.close(); f.kill() })
	withDashboard(t, f.main.root, map[string]any{"listen": "127.0.0.1:0"})
	f.main.start(t)
	c := f.client()
	var id control.ThreadID
	eventually(t, "the restarted main to admit the recorded worker", 15*time.Second, func() bool {
		var err error
		id, err = c.Start(context.Background(), control.StartRequest{Key: "dashboard-passive", Scope: "c1", Machine: "build", Repo: f.repo, Input: control.Input{Text: "screen dashboard worker output\nreply dashboard turn finished"}})
		if err != nil && !strings.Contains(err.Error(), "machine is not ready") {
			t.Fatal(err)
		}
		return err == nil
	})
	conv := f.main.thread(t, 0)
	f.main.posted(t, conv, "dashboard turn finished")
	f.ready(c, id)
	base, session := dashSignIn(t, f.main.root)
	first := len(trace.snapshot())
	stream, stop := openSSE(t, fmt.Sprintf("%s%s?thread=%d", base, dashboard.TerminalPath, id), session, "")
	var screen []byte
	deadline := time.After(10 * time.Second)
	for !bytes.Contains(screen, []byte("dashboard worker output")) {
		select {
		case e, ok := <-stream:
			if !ok {
				t.Fatal("recorded-worker terminal ended before output")
			}
			var ch control.TerminalChunk
			if err := json.Unmarshal([]byte(e.data), &ch); err != nil {
				t.Fatal(err)
			}
			if ch.Reset {
				screen = nil
			}
			screen = append(screen, ch.Data...)
		case <-deadline:
			t.Fatal("no output from recorded worker through dashboard")
		}
	}
	stop()
	for _, attempt := range []struct{ body, upgrade string }{{"forbidden dashboard keys", ""}, {"", "websocket"}} {
		r, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s%s?thread=%d", base, dashboard.TerminalPath, id), strings.NewReader(attempt.body))
		if err != nil {
			t.Fatal(err)
		}
		session.set(r)
		if attempt.upgrade != "" {
			r.Header.Set("Upgrade", attempt.upgrade)
			r.Header.Set("Connection", "Upgrade")
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("input-bearing passive request: HTTP %d", resp.StatusCode)
		}
	}
	seenRead := false
	for _, r := range trace.snapshot()[first:] {
		switch r.Op {
		case "attach", "keys", "resize", "input", "answer":
			t.Fatalf("passive dashboard caused worker operation %s", r.Op)
		case "read":
			if r.Target != nil && r.Target.Thread == uint64(id) {
				seenRead = true
			}
		}
	}
	if !seenRead {
		t.Fatal("dashboard did not read the recorded worker target")
	}
	// Only disposable fixture credentials are inspected, and failures never
	// print their contents. This checks selected browser responses, not all
	// credential/error paths or any external provider's credentials.
	key, err := os.ReadFile(filepath.Join(filepath.Dir(f.ssh.config), "client"))
	if err != nil {
		t.Fatal(err)
	}
	private := []string{f.root, f.main.root, "first-worker", f.binding.Main, f.binding.Worker, base64.StdEncoding.EncodeToString(key)}
	for _, line := range strings.Split(string(key), "\n") {
		if line != "" && !strings.HasPrefix(line, "-----") {
			private = append(private, line)
		}
	}
	for _, path := range []string{dashboard.ThreadsPath, fmt.Sprintf("%s?thread=%d", dashboard.ThreadPath, id)} {
		code, body := dashGet(t, base, session, path)
		if code != http.StatusOK {
			t.Fatalf("dashboard read: HTTP %d", code)
		}
		for _, marker := range private {
			if marker == "" || strings.Contains(body, marker) || bytes.Contains(screen, []byte(marker)) {
				t.Fatal("private worker connection or fixture credential data in browser response")
			}
		}
	}
	t.Log("actual isolated main/SSH/resident terminal read passed; no attach/keys/resize/input/answer requests during passive reads; fake agent, not a provider CLI capture")
}
