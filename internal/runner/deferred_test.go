package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

// This peer exercises the actual runner and host.Client, not the private host
// receiver. That receiver is tested independently with a controlled writer in
// host/input_batch_test.go. No process, terminal, CLI or provider is started.
type deferredPeer struct {
	mu         sync.Mutex
	run        string
	receipts   map[string]host.InputReceipt
	data       map[string][][]byte
	writes     int
	bytes      []byte
	lost, hold bool
}

func (p *deferredPeer) listen(t *testing.T, dir string) {
	t.Helper()
	ln, e := net.Listen("unix", host.SocketPath(dir))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go p.serve(c)
		}
	}()
}
func (p *deferredPeer) serve(c net.Conn) {
	defer c.Close()
	dec, enc := json.NewDecoder(c), json.NewEncoder(c)
	for {
		var req struct {
			Op    string           `json:"op"`
			Batch *host.InputBatch `json:"batch"`
		}
		if dec.Decode(&req) != nil {
			return
		}
		p.mu.Lock()
		reply := map[string]any{"ok": true}
		drop := false
		if req.Op == "input-capability" {
			reply["input_capability"] = host.InputCapability{Version: host.InputBatchVersion, Run: p.run}
		} else if req.Batch != nil && req.Batch.Run == p.run {
			b := req.Batch
			r, ok := p.receipts[b.Key]
			if !ok {
				r.State = "absent"
			}
			if req.Op == "input-batch" && !ok {
				r = host.InputReceipt{State: "pending", Digest: host.InputBatchDigest(b.Data)}
				p.data[b.Key] = b.Data
				if !p.hold {
					r = p.write(b.Key, r)
				}
				p.receipts[b.Key] = r
				if p.lost {
					p.lost = false
					drop = true
				}
			}
			reply["input_receipt"] = r
		} else {
			reply["error"] = "fixture operation or run refused"
		}
		p.mu.Unlock()
		if drop {
			return
		}
		if enc.Encode(reply) != nil {
			return
		}
	}
}
func (p *deferredPeer) write(key string, r host.InputReceipt) host.InputReceipt {
	p.writes++
	for _, b := range p.data[key] {
		p.bytes = append(p.bytes, b...)
	}
	r.State, r.CompletedAt = "written", time.Now().UnixMilli()
	return r
}
func (p *deferredPeer) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold = false
	for key, r := range p.receipts {
		if r.State == "pending" {
			p.receipts[key] = p.write(key, r)
		}
	}
}
func (p *deferredPeer) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.writes }

func deferredNative(t *testing.T) (before [][]byte, command, response, started, user, ended []byte, text string) {
	t.Helper()
	f, e := os.Open("../agent/codex/testdata/0.163.0/start/wire.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		var row struct {
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		var m struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
				Item struct {
					Type string `json:"type"`
				} `json:"item"`
			} `json:"params"`
		}
		if e = json.Unmarshal(s.Bytes(), &row); e != nil {
			t.Fatal(e)
		}
		json.Unmarshal(row.Msg, &m)
		line := append([]byte{}, row.Msg...)
		if row.Dir == "send" && m.Method == "turn/start" {
			command = line
			text = m.Params.Input[0].Text
			continue
		}
		if row.Dir != "recv" {
			continue
		}
		if command == nil {
			before = append(before, line)
			continue
		}
		if m.ID == 3 {
			response = line
		}
		switch m.Method {
		case "turn/started":
			started = line
		case "turn/completed":
			ended = line
		case "item/started":
			if m.Params.Item.Type == "userMessage" {
				user = line
			}
		}
	}
	if e = s.Err(); e != nil {
		t.Fatal(e)
	}
	if len(before) == 0 || command == nil || response == nil || user == nil || ended == nil {
		t.Fatal("incomplete actual capture")
	}
	return
}

func TestDeferredRecovery(t *testing.T) {
	for _, boundary := range []string{"accepted", "frozen", "lost-reply", "reserved", "ack-uncommitted", "ack-committed"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			before, _, response, started, user, _, text := deferredNative(t)
			ad, e := codex.New(nil)
			if e != nil {
				t.Fatal(e)
			}
			x := storedReader(t, ad.NewCodec(agent.Spec{}))
			r, lv := x.r, x.lv
			r.Machine = &session.Machine{Store: r.Store}
			lv.adapter = ad
			r.live = map[int64]*live{1: lv}
			p := &deferredPeer{run: "fixture-original-run", receipts: map[string]host.InputReceipt{}, data: map[string][][]byte{}, lost: boundary == "lost-reply", hold: boundary == "reserved"}
			p.listen(t, lv.dir)
			c := &core.Core{Store: r.Store, Machine: r.Machine, Runner: r, Log: r.Log, Scopes: map[string]core.Scope{"s": {Enabled: true}}, People: people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})}
			caller := control.Caller{From: "supervisor:s", AsksIn: 2, Scopes: []string{"s"}, May: control.OpRead | control.OpSend, OrdinaryOnly: true, RefuseBusy: true, DeferInput: true}
			c.Deferred = &core.DeferredInputs{Core: c, Policy: func(string, control.ThreadID) (control.Caller, []string, error) { return caller, []string{"repo"}, nil }, Transport: r.DeferredTransport}
			r.Deferred = c.Deferred
			r.RevalidatePending = c.RevalidatePending
			r.RevalidateInitial = c.RevalidateInitial
			for _, q := range []string{
				`UPDATE threads SET state='idle',repo='repo' WHERE id=1`,
				`UPDATE sessions SET state='idle' WHERE id=1`,
				`INSERT INTO workspaces(id,thread_id,path,remote,state) VALUES(1,1,'fixture-worker','repo','ready')`,
				`INSERT INTO threads(id,scope,key,started_by,state,confined,kind,created_at,updated_at) VALUES(2,'s','sv','owner','running',1,'supervisor',0,0)`,
				`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(2,2,'fake','running',0,0)`,
			} {
				if _, e = r.Store.DB.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
			readCommit := func() {
				t.Helper()
				b := r.read(x.lv)
				if b.err != nil {
					t.Fatal(b.err)
				}
				if _, e = r.commit(x.lv, b); e != nil {
					t.Fatal(e)
				}
				x.lv.offset = b.offset
				for _, a := range b.actions {
					if e = r.act(x.lv, a); e != nil {
						t.Fatal(e)
					}
				}
			}
			x.appendTo(filepath.Join(lv.dir, host.EventsFile), before...)
			readCommit()
			api := c.As(caller)
			req := control.DeferredSendRequest{Action: "queue", Target: 1, Key: "stable", Text: text}
			queued, e := api.DeferredSend(ctx, req)
			if e != nil {
				t.Fatal(e)
			}
			if boundary == "accepted" || boundary == "frozen" {
				r.point = func(point string) {
					if (boundary == "accepted" && point == "before pending dispatch") || (boundary == "frozen" && point == "deferred frozen") {
						panic("fixture daemon stops")
					}
				}
				func() {
					defer func() {
						if got := recover(); got != "fixture daemon stops" {
							t.Fatalf("wrong crash boundary: %v", got)
						}
					}()
					c.Deferred.DispatchOnce(ctx)
					t.Fatal("boundary was not reached")
				}()
				r.point = nil
			} else {
				c.Deferred.DispatchOnce(ctx)
			}
			if boundary == "ack-uncommitted" || boundary == "ack-committed" {
				x.appendTo(filepath.Join(lv.dir, host.EventsFile), response)
				if boundary == "ack-committed" {
					readCommit()
				} else {
					b := r.read(lv)
					if b.err != nil {
						t.Fatal(b.err)
					}
				}
			}
			for range 2 {
				// Rebuild the actual codec from committed retained reads, restore
				// Written correlation, commit new output, then reconcile and hand
				// over pending input. No native response or time budget is invented.
				x.lv = x.load(ad.NewCodec(agent.Spec{}))
				lv = x.lv
				lv.adapter = ad
				if e = r.Store.DB.QueryRow(`SELECT last_seq FROM sessions WHERE id=1`).Scan(&lv.seq); e != nil {
					t.Fatal(e)
				}
				r.live[1] = lv
				r.warm(lv)
				if lv.readErr != nil {
					t.Fatal(lv.readErr)
				}
				r.handOverWritten(lv)
				readCommit()
				if e = r.reconcileDeferred(lv); e != nil {
					t.Fatal(e)
				}
				if e = r.Store.DB.QueryRow(`SELECT coalesce(max(id),0) FROM pending_inputs WHERE session_id=1`).Scan(&lv.upTo); e != nil {
					t.Fatal(e)
				}
				r.handOverPending(lv)
				if boundary == "reserved" {
					if p.count() != 0 || !lv.deferredBlocked {
						t.Fatal("pending receiver was retried or allowed timeout")
					}
					if a := r.tickCodec(lv); len(a.Events) != 0 {
						t.Fatal("pending write expired")
					}
				}
			}
			if boundary == "reserved" {
				p.release()
				if e = r.reconcileDeferred(lv); e != nil {
					t.Fatal(e)
				}
			}
			if p.count() != 1 {
				t.Fatalf("physical fixture writes %d", p.count())
			}
			// A repeated same-text notification is not a matching request ACK.
			if boundary != "ack-committed" && boundary != "ack-uncommitted" {
				x.appendTo(filepath.Join(lv.dir, host.EventsFile), started, user, user)
				readCommit()
				got, e := api.DeferredSend(ctx, control.DeferredSendRequest{Action: "inspect", Number: queued.Number})
				if e != nil || got.State != "handoff" {
					t.Fatalf("false text ACK: %+v %v", got, e)
				}
				x.appendTo(filepath.Join(lv.dir, host.EventsFile), response)
				readCommit()
			}
			got, e := api.DeferredSend(ctx, control.DeferredSendRequest{Action: "inspect", Number: queued.Number})
			if e != nil || got.State != "delivered" {
				t.Fatalf("final receipt: %+v %v", got, e)
			}
			var pending, sessions int
			r.Store.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE session_id=1`).Scan(&pending)
			r.Store.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
			if pending != 0 || sessions != 2 {
				t.Fatalf("pending=%d sessions=%d", pending, sessions)
			}
			p.mu.Lock()
			var sent struct {
				Method string `json:"method"`
				ID     int    `json:"id"`
			}
			json.Unmarshal(bytes.TrimSpace(p.bytes), &sent)
			p.mu.Unlock()
			if sent.Method != "turn/start" || sent.ID != 3 {
				t.Fatalf("wrong frozen native submission: %+v", sent)
			}
		})
	}
}
