package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

// The complete real owned-host/follow proof belongs to CI. Local work uses
// controlled in-process peers only. This uses the existing capture reflector,
// never a coding CLI, authentication, inference, tmux or external service.
func TestDeferredOwnedHostRecoveryCI(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("actual owned-host process/follow proof runs in CI; local tests use in-process fixtures")
	}
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Fatal(e)
	}
	peer, e := filepath.Abs("testdata/codex_recovery.py")
	if e != nil {
		t.Fatal(e)
	}
	for _, boundary := range []string{"accepted", "frozen", "written"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			before, _, response, _, _, _, text := deferredNative(t)
			ad, e := codex.New(nil)
			if e != nil {
				t.Fatal(e)
			}
			x := storedReader(t, ad.NewCodec(agent.Spec{}))
			r, lv := x.r, x.lv
			r.Machine = &session.Machine{Store: r.Store}
			r.Host = host.Command{Path: os.Args[0], Args: []string{"role", "host"}}
			r.Poll, r.StartTimeout, r.IdlePeriod = 20*time.Millisecond, time.Minute, time.Hour
			lv.adapter = ad
			r.live = map[int64]*live{1: lv}
			spec := agent.Spec{StateDir: lv.dir}
			requests := filepath.Join(lv.dir, "deferred-fixture-requests.jsonl")
			w, e := r.own().Start(ctx, lv.dir, holder.Spec{Argv: []string{python, "-u", peer, requests}, Dir: lv.dir, Env: []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}})
			if e != nil {
				t.Fatal(e)
			}
			// The initial lv is record-only. Only real followers own close/done.
			following := false
			t.Cleanup(func() {
				if following {
					r.Close()
				}
				r.own().Stop(context.Background(), w, time.Second, time.Second)
			})
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
			writeCapture := func(line []byte) {
				t.Helper()
				wrapped, _ := json.Marshal(map[string]string{"fixture_response": string(bytes.TrimSpace(line))})
				if e := r.own().Write(ctx, w, [][]byte{append(wrapped, '\n')}); e != nil {
					t.Fatal(e)
				}
				waitFor(t, "supplied capture", 5*time.Second, func() bool {
					b, _ := os.ReadFile(filepath.Join(lv.dir, host.EventsFile))
					return bytes.Contains(b, bytes.TrimSpace(line))
				})
			}
			for _, line := range before {
				writeCapture(line)
			}
			b := r.read(lv)
			if b.err != nil {
				t.Fatal(b.err)
			}
			if _, e = r.commit(lv, b); e != nil {
				t.Fatal(e)
			}
			lv.offset = b.offset
			api := c.As(caller)
			queued, e := api.DeferredSend(ctx, control.DeferredSendRequest{Action: "queue", Target: 1, Key: "real-host-stable", Text: text})
			if e != nil {
				t.Fatal(e)
			}
			if boundary != "written" {
				r.point = func(point string) {
					if boundary == "accepted" && point == "before pending dispatch" || boundary == "frozen" && point == "deferred frozen" {
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
					t.Fatal("missing crash boundary")
				}()
				r.point = nil
			} else {
				c.Deferred.DispatchOnce(ctx)
			}
			countNative := func() int {
				raw, _ := os.ReadFile(requests)
				n := 0
				for _, line := range bytes.Split(raw, []byte("\n")) {
					var m struct{ Method string }
					if json.Unmarshal(line, &m) == nil && m.Method == "turn/start" {
						n++
					}
				}
				return n
			}
			for range 2 {
				if following {
					r.Close()
					following = false
				}
				if e = r.follow(1, ad, spec, reconnect); e != nil {
					t.Fatal(e)
				}
				following = true
				waitFor(t, "one real native submission", 5*time.Second, func() bool { return countNative() == 1 })
			}
			writeCapture(response)
			waitFor(t, "exact bound agent ACK", 5*time.Second, func() bool {
				got, e := api.DeferredSend(ctx, control.DeferredSendRequest{Action: "inspect", Number: queued.Number})
				return e == nil && got.State == "delivered"
			})
			r.Close()
			following = false
			var pending, sessions int
			r.Store.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE session_id=1`).Scan(&pending)
			r.Store.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
			if countNative() != 1 || pending != 0 || sessions != 2 {
				t.Fatalf("real-host submissions=%d pending=%d sessions=%d", countNative(), pending, sessions)
			}
		})
	}
}
