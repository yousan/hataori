package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// These fixtures execute the production pre-read probe, read/commit, batch
// barrier, reconciliation and handover functions without a follower/process.
// The peer is in-process; native messages are immutable retained captures.
type deferredProgressFixture struct {
	t     *testing.T
	x     *reader
	c     *core.Core
	api   control.API
	owner control.API
	ad    agent.Adapter
	spec  agent.Spec
	p     *deferredPeer
	q     control.DeferredSend
	text  string
}

func newDeferredProgress(t *testing.T, scenario string, policy agent.Policy) *deferredProgressFixture {
	t.Helper()
	before, _, _, _, _, _, text := deferredNativeScenario(t, scenario)
	ad, e := codex.New(nil)
	if e != nil {
		t.Fatal(e)
	}
	spec := agent.Spec{Approvals: policy}
	x := storedReader(t, ad.NewCodec(spec))
	r, lv := x.r, x.lv
	r.Machine = &session.Machine{Store: r.Store}
	lv.adapter, lv.spec = ad, spec
	r.live = map[int64]*live{1: lv}
	p := &deferredPeer{run: "fixture-original-run", receipts: map[string]host.InputReceipt{}, data: map[string][][]byte{}}
	p.listen(t, lv.dir)
	c := &core.Core{Store: r.Store, Machine: r.Machine, Runner: r, Log: r.Log, Scopes: map[string]core.Scope{"s": {Enabled: true}}, People: people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:s"}}}, "owner", map[string][]string{"s": {"sv"}})}
	caller := control.Caller{From: "supervisor:s", AsksIn: 2, Scopes: []string{"s"}, May: control.OpRead | control.OpSend, OrdinaryOnly: true, RefuseBusy: true, DeferInput: true}
	c.Deferred = &core.DeferredInputs{Core: c, Policy: func(string, control.ThreadID) (control.Caller, []string, error) { return caller, []string{"repo"}, nil }, Transport: r.DeferredTransport}
	r.Deferred, r.RevalidatePending, r.RevalidateInitial = c.Deferred, c.RevalidatePending, c.RevalidateInitial
	for _, sql := range []string{
		`UPDATE threads SET state='idle',repo='repo' WHERE id=1`,
		`UPDATE sessions SET state='idle' WHERE id=1`,
		`INSERT INTO workspaces(id,thread_id,path,remote,state) VALUES(1,1,'fixture-worker','repo','ready')`,
		`INSERT INTO threads(id,scope,key,started_by,state,confined,kind,created_at,updated_at) VALUES(2,'s','sv','owner','running',1,'supervisor',0,0)`,
		`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(2,2,'fake','running',0,0)`,
	} {
		if _, e = r.Store.DB.Exec(sql); e != nil {
			t.Fatal(e)
		}
	}
	f := &deferredProgressFixture{t: t, x: x, c: c, api: c.As(caller), owner: c.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: control.OpRead | control.OpSend | control.OpAnswer}), ad: ad, spec: spec, p: p, text: text}
	f.append(before...)
	f.pump()
	return f
}

func (f *deferredProgressFixture) append(lines ...[]byte) {
	f.t.Helper()
	f.x.appendTo(filepath.Join(f.x.lv.dir, host.EventsFile), lines...)
}
func (f *deferredProgressFixture) state() string {
	f.t.Helper()
	q, e := f.api.DeferredSend(context.Background(), control.DeferredSendRequest{Action: "inspect", Number: f.q.Number})
	if e != nil {
		f.t.Fatal(e)
	}
	return q.State
}
func (f *deferredProgressFixture) queueCrash(point string) {
	f.t.Helper()
	q, e := f.api.DeferredSend(context.Background(), control.DeferredSendRequest{Action: "queue", Target: 1, Key: "stable", Text: f.text})
	if e != nil {
		f.t.Fatal(e)
	}
	f.q = q
	r := f.x.r
	r.point = func(got string) {
		if got == point {
			panic("fixture daemon stops")
		}
	}
	func() {
		defer func() {
			if recover() != "fixture daemon stops" {
				f.t.Fatal("wrong crash boundary")
			}
		}()
		f.c.Deferred.DispatchOnce(context.Background())
		f.t.Fatal("missing crash boundary")
	}()
	r.point = nil
	lv := f.x.load(f.ad.NewCodec(f.spec))
	lv.adapter, lv.spec, lv.warmPending, lv.pending = f.ad, f.spec, true, true
	r.Store.DB.QueryRow(`SELECT last_seq FROM sessions WHERE id=1`).Scan(&lv.seq)
	r.Store.DB.QueryRow(`SELECT coalesce(max(id),0) FROM pending_inputs WHERE session_id=1`).Scan(&lv.upTo)
	f.x.lv, r.live[1] = lv, lv
}

func (f *deferredProgressFixture) pump() bool {
	f.t.Helper()
	r, lv := f.x.r, f.x.lv
	if lv.warmPending {
		lv.codec = f.ad.NewCodec(f.spec)
		r.warm(lv)
		if lv.readErr != nil {
			f.t.Fatal(lv.readErr)
		}
		lv.warmPending = false
		r.handOverWritten(lv)
	}
	ready, e := r.beforeDeferredRead(lv)
	if e != nil {
		f.t.Fatal(e)
	}
	if !ready {
		return false
	}
	b := r.read(lv)
	if b.err != nil {
		f.t.Fatal(b.err)
	}
	if _, e = r.commit(lv, b); e != nil {
		f.t.Fatal(e)
	}
	lv.offset = b.offset
	for p, o := range b.files {
		lv.files[p] = o
	}
	if !r.runReadActions(lv, b.actions) {
		return false
	}
	if lv.pending {
		lv.pending = false
		r.handOverPending(lv)
	}
	return true
}

func nativeApproval(t *testing.T, scenario string) []byte {
	t.Helper()
	f, e := os.Open(filepath.Join("../agent/codex/testdata/0.163.0", scenario, "wire.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		var row struct {
			Dir string
			Msg json.RawMessage
		}
		var msg struct{ Method string }
		if json.Unmarshal(s.Bytes(), &row) != nil || json.Unmarshal(row.Msg, &msg) != nil {
			t.Fatal("bad capture")
		}
		if row.Dir == "recv" && msg.Method == "item/commandExecution/requestApproval" {
			return append([]byte(nil), row.Msg...)
		}
	}
	if e = s.Err(); e != nil {
		t.Fatal(e)
	}
	t.Fatal("actual approval request missing")
	return nil
}

func TestDeferredAbsentQuestionAllowsHumanAnswer(t *testing.T) {
	f := newDeferredProgress(t, "approval_accept", agent.AskInChat)
	_, _, _, started, user, _, _ := deferredNativeScenario(t, "approval_accept")
	f.queueCrash("deferred frozen")
	f.append(started, user, nativeApproval(t, "approval_accept"))
	f.pump()
	if f.state() != "failed" || f.p.count() != 0 {
		t.Fatal("new question did not fail absent instruction", f.state(), f.p.count())
	}
	f.pump() // reconstruct the withdrawn input, retaining the real open native request
	var handle control.RequestID
	if e := f.x.r.Store.DB.QueryRow(`SELECT handle FROM requests WHERE session_id=1 AND state='open'`).Scan(&handle); e != nil {
		t.Fatal(e)
	}
	if e := f.owner.Answer(context.Background(), handle, control.Answer{Allow: true}); e != nil {
		t.Fatal(e)
	}
	if f.p.ordinaryCount() != 1 {
		t.Fatal("human answer was fenced", f.p.ordinaryCount())
	}
}

func TestDeferredAbsentNativeProgress(t *testing.T) {
	for _, boundary := range []string{"active", "dirty-idle", "pending"} {
		t.Run(boundary, func(t *testing.T) {
			f := newDeferredProgress(t, "approval_accept", agent.AllowAll)
			_, _, _, started, user, ended, _ := deferredNativeScenario(t, "approval_accept")
			if boundary == "pending" {
				f.p.hold = true
			}
			f.queueCrash("deferred frozen")
			if boundary == "pending" {
				f.pump()
				if f.p.count() != 0 {
					t.Fatal("pending fixture wrote early")
				}
			}
			f.append(started, user)
			if boundary == "dirty-idle" {
				f.append(ended)
			}
			f.append(nativeApproval(t, "approval_accept"))
			old := f.x.lv.offset
			f.pump()
			if boundary == "pending" {
				if f.x.lv.offset != old || f.p.ordinaryCount() != 0 {
					t.Fatal("pending receiver consumed native one-shot reply")
				}
				f.p.release()
				f.pump()
			}
			if f.p.ordinaryCount() != 1 {
				t.Fatal("ordinary native approval reply was lost", f.p.ordinaryCount())
			}
			if boundary != "pending" && f.p.count() != 0 {
				t.Fatal("absent instruction crossed native/batch progress", f.p.count())
			}
			if boundary == "active" {
				a := f.x.r.tickCodec(f.x.lv)
				if e := f.x.r.do(f.x.lv, a); e != nil {
					t.Fatal(e)
				}
				// Only A's unstarted deadline is suspended; ordinary codec progress remains callable.
				a = f.x.lv.codec.Tick(time.Now().Add(time.Hour))
				for _, e := range a.Events {
					if e.Kind == agent.TurnEnded {
						t.Fatal("unwritten frozen request expired")
					}
				}
				f.append(ended)
			}
			f.pump()
			f.pump()
			if f.p.count() != 1 || f.p.ordinaryCount() != 1 {
				t.Fatal("progress/recovery duplicated or stranded writes", f.p.count(), f.p.ordinaryCount())
			}
		})
	}
}

func TestDeferredSameBatchProgressDoesNotFreeze(t *testing.T) {
	f := newDeferredProgress(t, "approval_accept", agent.AllowAll)
	_, _, _, started, user, ended, _ := deferredNativeScenario(t, "approval_accept")
	f.queueCrash("before pending dispatch")
	f.append(started, user)
	f.pump() // A is codec-held behind this late native turn
	f.append(ended, nativeApproval(t, "approval_accept"))
	f.pump()
	if f.state() != "accepted" || f.p.count() != 0 || f.p.ordinaryCount() != 1 {
		t.Fatal("dirty batch crossed freeze or lost its reply", f.state(), f.p.count(), f.p.ordinaryCount())
	}
	f.pump()
	f.pump()
	if f.p.count() != 1 || f.p.ordinaryCount() != 1 {
		t.Fatal("fresh reconstruction did not submit once", f.p.count(), f.p.ordinaryCount())
	}
}

func TestDeferredPreFreezeHeldInputSurvivesPendingReceipt(t *testing.T) {
	f := newDeferredProgress(t, "start", agent.AllowAll)
	_, _, _, lateStart, lateUser, lateEnd, _ := deferredNativeScenario(t, "resume")
	_, _, _, started, user, _, _ := deferredNative(t)
	f.p.hold = true
	f.queueCrash("before pending dispatch")
	f.append(lateStart, lateUser)
	f.pump()
	if e := f.owner.Send(context.Background(), 1, "ordinary-before-freeze", control.Input{Text: "ordinary held before freeze"}, control.Deliver); e != nil {
		t.Fatal(e)
	}
	f.append(lateEnd)
	f.pump()
	f.pump()
	f.pump()
	if f.p.count() != 0 {
		t.Fatal("pending receiver wrote early")
	}
	f.p.release()
	f.pump()
	f.pump()
	f.append(started, user)
	f.pump()
	if e := f.x.r.do(f.x.lv, f.x.r.tickCodec(f.x.lv)); e != nil {
		t.Fatal(e)
	}
	if f.p.count() != 1 || f.p.ordinaryCount() != 1 {
		t.Fatal("pre-freeze held input duplicated/stranded", f.p.count(), f.p.ordinaryCount())
	}
}
