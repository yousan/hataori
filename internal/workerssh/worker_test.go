package workerssh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

func TestPrivateForwardSharesMasterAcrossIndependentSessions(t *testing.T) {
	f := newSSHFixture(t)
	pool := f.pool(Options{})
	w := f.ready(pool, f.config())
	same, err := pool.Worker(f.config())
	if err != nil || same != w {
		t.Fatalf("destination did not share owner: %v", err)
	}
	w.mu.Lock()
	g := w.current
	pid := g.command.Process.Pid
	w.mu.Unlock()
	for _, path := range []string{g.ctl, g.forward} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("private socket: %v %v", info, err)
		}
	}
	a, err := w.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := w.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	targets := []workerproto.Target{{Thread: 1, Session: "session-first", Run: "run-first"}, {Thread: 2, Session: "session-second", Run: "run-second"}}
	f.mu.Lock()
	f.expected = targets
	f.mu.Unlock()
	for i, stream := range []*Stream{a, b} {
		reply := roundTrip(t, stream, request(f.binding, "prepare", fmt.Sprintf("prepare-%d", i), &targets[i]))
		var receipt workerproto.Receipt
		if json.Unmarshal(reply.Body, &receipt) != nil || receipt.State != "complete" {
			t.Fatalf("independent session: %s", reply.Body)
		}
	}
	a.Close()
	reply := roundTrip(t, b, request(f.binding, "health", "sibling-still-open", nil))
	if reply.Kind != "reply" {
		t.Fatal(reply)
	}
	eventually(t, "shared connection remains ready", func() bool { return w.Status().Ready })
	w.mu.Lock()
	current := w.current
	w.mu.Unlock()
	if current != g || current.command.Process.Pid != pid {
		t.Fatal("independent stream created another master")
	}
	t.Log("ran: system OpenSSH -> private Unix forward -> real resident; independent session streams, no remote command")
}

func TestConnectionCutRequiresReconciliationBeforeReady(t *testing.T) {
	f := newSSHFixture(t)
	var allow atomic.Bool
	allow.Store(true)
	var reconciliations atomic.Int32
	cfg := f.config()
	cfg.Reconcile = func(ctx context.Context, o Observation) error {
		reconciliations.Add(1)
		if !allow.Load() {
			return fmt.Errorf("main reconciliation refused")
		}
		return nil
	}
	w := f.ready(f.pool(Options{}), cfg)
	stream, err := w.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first := w.Status().Generation
	allow.Store(false)
	w.mu.Lock()
	owned := w.current.command.Process
	w.mu.Unlock()
	// Cut only the client process created by this worker generation, preserving
	// the fixture server and resident. It cannot be mistaken for a host stop.
	if err := owned.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cut shared link becomes unavailable", func() bool { return !w.Status().Ready })
	stream.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("old stream survived link cut")
	}
	eventually(t, "new connection is still gated by main reconciliation", func() bool { s := w.Status(); return s.Generation > first && s.Cause == Reconciliation && !s.Ready })
	allow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatal(err, w.Status())
	}
	if w.Status().Generation <= first || reconciliations.Load() < 2 {
		t.Fatal("reconnect skipped reconciliation")
	}
	next, err := w.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if reply := roundTrip(t, next, request(f.binding, "inventory", "after-cut", nil)); reply.Kind != "reply" {
		t.Fatal(reply)
	}
	t.Log("ran: owned SSH connection cut; generation streams close; bounded retry; binding/version/health/inventory/main reconciliation gates readiness")
}

func TestSSHFailuresStayUnavailable(t *testing.T) {
	for _, kind := range []Cause{HostKey, Authentication, Forward} {
		t.Run(string(kind), func(t *testing.T) {
			var extra []string
			if kind == Forward {
				extra = []string{"AllowStreamLocalForwarding no"}
			}
			f := newSSHFixture(t, extra...)
			if kind == HostKey {
				writePrivate(t, filepath.Join(f.dir, "known_hosts"), nil)
			}
			if kind == Authentication {
				f.writeClient("wrong")
			}
			var captured atomic.Bool
			options := Options{capture: func(c Cause, b []byte) {
				if c != kind || len(b) == 0 || *captureSSH == "" || !captured.CompareAndSwap(false, true) {
					return
				}
				// Redact only fixture paths, the OS login name and generated loopback
				// port. No operator destination/credential was consulted by this run.
				text := strings.ReplaceAll(string(b), f.dir, "<fixture-root>")
				text = strings.ReplaceAll(text, f.user, "fixture-user")
				text = strings.ReplaceAll(text, fmt.Sprintf("%d", f.port), "<fixture-port>")
				text = regexp.MustCompile(`/tmp/hataori-workerssh-[^/\s]+`).ReplaceAllString(text, "<transport-dir>")
				if err := os.MkdirAll(*captureSSH, 0700); err != nil {
					t.Error(err)
					return
				}
				file, err := os.OpenFile(filepath.Join(*captureSSH, string(kind)+".stderr"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					t.Error(err)
					return
				}
				defer file.Close()
				if _, err := file.Write([]byte(text)); err != nil {
					t.Error(err)
				}
			}}
			w, err := f.pool(options).Worker(f.config())
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "visible bounded "+string(kind)+" failure", func() bool { s := w.Status(); return !s.Ready && s.Cause == kind })
			if _, err := w.Open(context.Background()); err == nil {
				t.Fatal("failed transport admitted stream")
			}
			if *captureSSH != "" {
				eventually(t, "captured actual OpenSSH failure", func() bool { return captured.Load() })
			}
			paths, err := filepath.Glob(filepath.Join(f.remoteRoot, "state/sessions/*/host.json"))
			if err != nil || len(paths) != 0 {
				t.Fatal("transport failure launched an agent", paths, err)
			}
			t.Log("ran: isolated OpenSSH failure", kind, "; no agent launch and no skipped prerequisites")
		})
	}
}
