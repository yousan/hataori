package workerssh

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/worker"
	"github.com/yousan/hataori/internal/workerproto"
)

// This peer emits the public worker protocol, not invented agent output. SSH
// still crosses the fixture server and its real private streamlocal forward.
func (f *sshFixture) replacePeer(change func(*workerproto.Hello, *Observation), wrongID bool) *atomic.Int32 {
	f.t.Helper()
	queries := new(atomic.Int32)
	if err := f.resident.Close(); err != nil {
		f.t.Fatal(err)
	}
	listener, err := net.Listen("unix", worker.SocketPath(f.remoteRoot))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(worker.SocketPath(f.remoteRoot), 0600); err != nil {
		f.t.Fatal(err)
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var wg sync.WaitGroup
	stopped := false
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if stopped {
				mu.Unlock()
				c.Close()
				return
			}
			connections[c] = true
			wg.Add(1)
			mu.Unlock()
			go func() {
				defer wg.Done()
				defer func() { c.Close(); mu.Lock(); delete(connections, c); mu.Unlock() }()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(c)
				if _, err := reader.ReadBytes('\n'); err != nil {
					return
				}
				hello := f.resident.Hello()
				observation := Observation{Health: workerproto.Health{Boot: "boot-fixture", Ready: true, Agents: []workerproto.AgentHealth{}, Telemetry: workerproto.Telemetry{}}, Inventory: workerproto.Inventory{Boot: "boot-fixture", Complete: true, Hosts: []workerproto.HostRecord{}}}
				change(&hello, &observation)
				data, _ := json.Marshal(hello)
				if _, err := c.Write(append(data, '\n')); err != nil {
					return
				}
				for {
					data, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var request workerproto.Message
					if json.Unmarshal(data, &request) != nil {
						return
					}
					queries.Add(1)
					var body any
					switch request.Op {
					case "health":
						body = observation.Health
					case "inventory":
						body = observation.Inventory
					default:
						return
					}
					raw, _ := json.Marshal(body)
					reply := workerproto.Message{Kind: "reply", ID: request.ID, Binding: f.binding, Op: request.Op, Body: raw}
					if wrongID {
						reply.ID = "wrong-correlation"
					}
					if writeFrame(c, reply) != nil {
						return
					}
				}
			}()
		}
	}()
	f.t.Cleanup(func() {
		mu.Lock()
		stopped = true
		for c := range connections {
			c.Close()
		}
		mu.Unlock()
		listener.Close()
		wg.Wait()
	})
	return queries
}

func TestReadinessRejectsUnverifiedPeer(t *testing.T) {
	cases := []struct {
		name    string
		cause   Cause
		change  func(*workerproto.Hello, *Observation)
		wrongID bool
	}{
		{"incompatible_major", Protocol, func(h *workerproto.Hello, _ *Observation) { h.Version = "2.0" }, false},
		{"wrong_binding", Binding, func(h *workerproto.Hello, _ *Observation) { h.Binding.Worker = "other-installation" }, false},
		{"health_not_ready", Reconciliation, func(_ *workerproto.Hello, o *Observation) { o.Health.Ready = false }, false},
		{"incomplete_inventory", Reconciliation, func(_ *workerproto.Hello, o *Observation) { o.Inventory.Complete = false }, false},
		{"changed_boot", Reconciliation, func(_ *workerproto.Hello, o *Observation) { o.Inventory.Boot = "other-boot" }, false},
		{"unregistered_host", Reconciliation, func(_ *workerproto.Hello, o *Observation) {
			o.Inventory.Hosts = []workerproto.HostRecord{{Target: workerproto.Target{Thread: 9, Session: "other-session", Run: "other-run"}, State: "running"}}
		}, false},
		{"wrong_reply_id", Reconciliation, func(*workerproto.Hello, *Observation) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSHFixture(t)
			queries := f.replacePeer(tc.change, tc.wrongID)
			var reconciled atomic.Int32
			cfg := f.config()
			cfg.Reconcile = func(context.Context, Observation) error { reconciled.Add(1); return nil }
			pool := f.pool(Options{BackoffMin: time.Second, BackoffMax: time.Second})
			w, err := pool.Worker(cfg)
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "specific peer refusal", func() bool { return w.Status().Cause == tc.cause })
			// The master is terminated after the refusal. Preserve its specific cause
			// throughout backoff rather than overwriting it with generic Transport.
			time.Sleep(300 * time.Millisecond)
			if status := w.Status(); status.Ready || status.Cause != tc.cause {
				t.Fatalf("refusal overwritten during teardown: %+v", status)
			}
			if reconciled.Load() != 0 {
				t.Fatal("unverified peer reached main reconciliation")
			}
			if (tc.cause == Binding || tc.cause == Protocol) && queries.Load() != 0 {
				t.Fatal("binding/version refusal did not precede worker requests")
			}
			if _, err := w.Open(context.Background()); err == nil {
				t.Fatal("unverified peer admitted session")
			}
			t.Log("ran: real SSH forward; bounded", tc.name, "refusal before main reconciliation or agent launch")
		})
	}
}
