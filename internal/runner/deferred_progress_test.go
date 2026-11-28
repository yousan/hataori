package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	r, lv := f.x.r, f.x.lv
	r.IdlePeriod = time.Nanosecond
	r.point = func(point string) {
		if point == "before idle stop" {
			panic("accepted deferred input lost its receiver before reconstruction")
		}
	}
	defer func() {
		if p := recover(); p != nil {
			t.Fatal(p) // abort before any stop/PID action in a negative test
		}
	}()
	r.checkIdle(lv)
	if slept, e := r.Sleep(context.Background(), 1); e != nil || slept || lv.asleep || !lv.drain.IsZero() {
		t.Fatal("accepted input was put to sleep before fresh handover", slept, e)
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

func TestDeferredEndedReceiverDrainsBeforeExit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ack, written bool
	}{{"unknown-unconfirmed", false, false}, {"unknown-exact-ACK", true, false}, {"written-unconfirmed", false, true}, {"written-exact-ACK", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeferredProgress(t, "approval_accept", agent.AllowAll)
			_, _, response, started, user, _, _ := deferredNativeScenario(t, "approval_accept")
			f.queueCrash("deferred frozen")
			writes := 0
			if tc.written {
				f.pump()
				writes = 1
			}
			if e := f.p.closePeer(); e != nil {
				t.Fatal(e)
			}
			// Only our disposable peer is closed. A recorded exit and absent
			// socket exercise the real owned-holder liveness, without a PID.
			if e := os.WriteFile(filepath.Join(f.x.lv.dir, host.StatusFile), []byte(`{"exit_code":0}`), 0600); e != nil {
				t.Fatal(e)
			}
			if !f.x.r.polled(f.x.lv.dir).Ended() {
				t.Fatal("fixture receiver is not positively ended")
			}
			if tc.ack {
				f.append(response, started, user)
			} else {
				// A real native request would normally emit a one-shot reply.
				// Terminal drain must commit evidence without executing it.
				f.append(started, user, nativeApproval(t, "approval_accept"))
			}
			if !f.x.r.step(f.x.lv) || !f.x.lv.deferredDrain {
				t.Fatal("final native evidence was not drained before exit")
			}
			want := "handoff"
			if tc.ack {
				want = "delivered"
			}
			if f.state() != want {
				t.Fatal("terminal drain lost exact evidence", f.state(), want)
			}
			// An empty committed drain must reach normal Exit, not rebuild
			// forever. No native SessionEnded message is present in this test.
			if f.x.r.step(f.x.lv) || !f.x.lv.ended {
				t.Fatal("ended receiver remained in a recovery loop")
			}
			if !tc.ack {
				want = "failed"
			}
			if f.state() != want || f.p.count() != writes || f.p.ordinaryCount() != 0 {
				t.Fatal("terminal drain changed receipt or performed I/O", f.state(), f.p.count(), f.p.ordinaryCount())
			}
			var n int
			if e := f.x.r.Store.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); e != nil || n != 2 {
				t.Fatal("terminal drain created a replacement", n, e)
			}
		})
	}
}

func TestDeferredFailureWithoutReceiver(t *testing.T) {
	f := newDeferredProgress(t, "start", agent.AllowAll)
	f.queueCrash("deferred frozen")
	if e := f.p.closePeer(); e != nil {
		t.Fatal(e)
	}
	if e := f.x.r.failDeferred(f.x.lv, "deferred:1", "fixture terminal failure"); e != nil {
		t.Fatal("event-only refusal attempted to reach missing receiver", e)
	}
	if f.state() != "failed" {
		t.Fatal(f.state())
	}
}

func TestDeferredFailurePropagatesCommitError(t *testing.T) {
	f := newDeferredProgress(t, "start", agent.AllowAll)
	f.queueCrash("deferred frozen")
	f.p.refuse = true // repeatable real receiver refusal, never a new arbitrary decision
	r, lv := f.x.r, f.x.lv
	r.warm(lv)
	lv.warmPending = false
	r.handOverWritten(lv)
	if _, e := f.x.r.Store.DB.Exec(`CREATE TRIGGER fixture_refusal_fault BEFORE UPDATE OF last_seq ON sessions WHEN NEW.id=1 AND NEW.last_seq!=OLD.last_seq BEGIN SELECT RAISE(ABORT,'fixture refusal commit failure'); END`); e != nil {
		t.Fatal(e)
	}
	if e := r.reconcileDeferred(lv); e == nil {
		t.Fatal("refusal event commit failure was swallowed")
	}
	if !f.x.lv.warmPending || !f.x.lv.deferredBlocked {
		t.Fatal("consumed refusal was not fenced for reconstruction")
	}
	if f.state() != "handoff" {
		t.Fatal("failed cleanup committed receipt separately", f.state())
	}
	if _, e := f.x.r.Store.DB.Exec(`DROP TRIGGER fixture_refusal_fault`); e != nil {
		t.Fatal(e)
	}
	// The same original receiver now explicitly refuses, without writing.
	// Normal step reconstruction must retry and atomically finish cleanup.
	f.x.r.step(f.x.lv)
	f.x.r.step(f.x.lv)
	var pending int
	var state string
	if e := f.x.r.Store.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE session_id=1`).Scan(&pending); e != nil {
		t.Fatal(e)
	}
	if e := f.x.r.Store.DB.QueryRow(`SELECT state FROM sessions WHERE id=1`).Scan(&state); e != nil {
		t.Fatal(e)
	}
	if f.state() != "failed" || pending != 0 || state != "idle" || f.p.count() != 0 {
		t.Fatal("fault-cleared refusal did not recover atomically", f.state(), pending, state, f.p.count())
	}
	q, e := f.api.DeferredSend(context.Background(), control.DeferredSendRequest{Action: "inspect", Number: f.q.Number})
	if e != nil || !strings.Contains(q.Reason, "receiver explicitly refused") {
		t.Fatal("recovered refusal lost actual reason", q.Reason, e)
	}
}

func TestDeferredEndedDrainRetriesExitCommit(t *testing.T) {
	f := newDeferredProgress(t, "start", agent.AllowAll)
	f.queueCrash("deferred frozen")
	if e := f.p.closePeer(); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(f.x.lv.dir, host.StatusFile), []byte(`{"exit_code":0}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := f.x.r.Store.DB.Exec(`CREATE TRIGGER fixture_exit_fault BEFORE UPDATE ON deferred_sends WHEN NEW.state='failed' BEGIN SELECT RAISE(ABORT,'fixture exit commit failure'); END`); e != nil {
		t.Fatal(e)
	}
	if !f.x.r.step(f.x.lv) || f.x.lv.ended || f.state() != "handoff" {
		t.Fatal("follower ended before terminal transaction committed")
	}
	if _, e := f.x.r.Store.DB.Exec(`DROP TRIGGER fixture_exit_fault`); e != nil {
		t.Fatal(e)
	}
	if f.x.r.step(f.x.lv) || !f.x.lv.ended || f.state() != "failed" || f.p.count() != 0 {
		t.Fatal("terminal transaction did not recover without I/O")
	}
}

func TestDeferredDeniedAbsentRecoversCleanupFault(t *testing.T) {
	for _, cause := range []string{"question", "authority"} {
		t.Run(cause, func(t *testing.T) {
			f := newDeferredProgress(t, "approval_accept", agent.AskInChat)
			f.queueCrash("deferred frozen")
			r, lv := f.x.r, f.x.lv
			r.warm(lv)
			lv.warmPending = false
			r.handOverWritten(lv)
			if cause == "question" {
				_, _, _, started, user, _, _ := deferredNativeScenario(t, "approval_accept")
				f.append(started, user, nativeApproval(t, "approval_accept"))
				b := r.read(lv)
				if b.err != nil {
					t.Fatal(b.err)
				}
				if _, e := r.commit(lv, b); e != nil {
					t.Fatal(e)
				}
				lv.offset = b.offset
				for p, off := range b.files {
					lv.files[p] = off
				}
			} else {
				f.c.Scopes["s"] = core.Scope{Enabled: false}
			}
			if _, e := r.Store.DB.Exec(`CREATE TRIGGER fixture_denied_cleanup BEFORE UPDATE OF last_seq ON sessions WHEN NEW.id=1 AND NEW.last_seq!=OLD.last_seq BEGIN SELECT RAISE(ABORT,'fixture denied cleanup fault'); END`); e != nil {
				t.Fatal(e)
			}
			if e := r.reconcileDeferred(lv); e == nil {
				t.Fatal("denied cleanup fault was swallowed")
			}
			if f.state() != "handoff" || !lv.warmPending || f.p.count() != 0 {
				t.Fatal("preliminary denial escaped atomic cleanup", f.state(), lv.warmPending, f.p.count())
			}
			if _, e := r.Store.DB.Exec(`DROP TRIGGER fixture_denied_cleanup`); e != nil {
				t.Fatal(e)
			}
			r.step(lv)
			r.step(lv)
			var pending int
			if e := r.Store.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE session_id=1`).Scan(&pending); e != nil {
				t.Fatal(e)
			}
			if f.state() != "failed" || pending != 0 || f.p.count() != 0 {
				t.Fatal("denial failed to recover cleanup", f.state(), pending, f.p.count())
			}
			if cause == "question" {
				var handle control.RequestID
				if e := r.Store.DB.QueryRow(`SELECT handle FROM requests WHERE session_id=1 AND state='open'`).Scan(&handle); e != nil {
					t.Fatal(e)
				}
				if e := f.owner.Answer(context.Background(), handle, control.Answer{Allow: true}); e != nil {
					t.Fatal(e)
				}
				if f.p.ordinaryCount() != 1 || f.p.count() != 0 {
					t.Fatal("deferred denial consumed the human answer")
				}
			}
		})
	}
}

func TestDeferredHandoffPreventsSleep(t *testing.T) {
	for _, slot := range []string{"pending", "first"} {
		for _, receipt := range []string{"accepted", "unknown", "written", "lookup-error"} {
			t.Run(slot+"/"+receipt, func(t *testing.T) {
				f := newDeferredProgress(t, "start", agent.AllowAll)
				point := "deferred frozen"
				if receipt == "accepted" {
					point = "before pending dispatch"
				}
				f.queueCrash(point)
				if receipt == "written" {
					f.pump()
				}
				r, lv := f.x.r, f.x.lv
				// State-only logical idle after a distinct native turn ended.
				// The frozen input still awaits its own exact acknowledgement.
				for _, q := range []string{`UPDATE sessions SET state='idle',updated_at=1 WHERE id=1`, `UPDATE threads SET state='idle' WHERE id=1`, `UPDATE turns SET ended_at=1,status='completion' WHERE session_id=1 AND ended_at IS NULL`} {
					if _, e := r.Store.DB.Exec(q); e != nil {
						t.Fatal(e)
					}
				}
				if slot == "first" {
					// State-only resumed-first binding. No process is resumed.
					first, e := json.Marshal(firstInput{Key: fmt.Sprintf("deferred:%d", f.q.Number), Via: control.ViaDeferred, Sent: true, Written: receipt != "accepted", Text: f.text})
					if e != nil {
						t.Fatal(e)
					}
					for _, q := range []struct {
						sql  string
						args []any
					}{{`UPDATE sessions SET first_input=? WHERE id=1`, []any{string(first)}}, {`UPDATE deferred_sends SET input_ref='first' WHERE id=?`, []any{f.q.Number}}, {`DELETE FROM pending_inputs WHERE session_id=1`, nil}} {
						if _, e := r.Store.DB.Exec(q.sql, q.args...); e != nil {
							t.Fatal(e)
						}
					}
				}
				if receipt == "lookup-error" {
					if _, e := r.Store.DB.Exec(`UPDATE deferred_sends SET handoff='{' WHERE id=?`, f.q.Number); e != nil {
						t.Fatal(e)
					}
				}
				r.IdlePeriod = time.Nanosecond
				r.point = func(point string) {
					if point == "before idle stop" {
						panic("idle stop attempted before deferred receipt resolution")
					}
				}
				defer func() {
					if p := recover(); p != nil {
						t.Fatal(p) // negative test stops before any stop/PID action
					}
				}()
				r.checkIdle(lv)
				if lv.asleep || !lv.drain.IsZero() {
					t.Fatal("automatic idle discarded the original receiver")
				}
				slept, e := r.Sleep(context.Background(), 1)
				if slept || !lv.drain.IsZero() || (receipt == "lookup-error" && e == nil) {
					t.Fatal("explicit sleep discarded unresolved handoff", slept, e)
				}
			})
		}
	}
}

func TestDeferredAcceptedSleepGuardKeepsOriginBinding(t *testing.T) {
	f := newDeferredProgress(t, "start", agent.AllowAll)
	f.queueCrash("before pending dispatch")
	if found, e := f.c.Deferred.HasAcceptedInput(context.Background(), 1); e != nil || !found {
		t.Fatal("accepted current input not protected", found, e)
	}
	if found, e := f.c.Deferred.HasAcceptedInput(context.Background(), 2); e != nil || found {
		t.Fatal("another session inherited the accepted input", found, e)
	}
	if _, e := f.x.r.Store.DB.Exec(`UPDATE pending_inputs SET data=json_set(data,'$.origin.key','ordinary-reused-slot') WHERE session_id=1`); e != nil {
		t.Fatal(e)
	}
	if found, e := f.c.Deferred.HasAcceptedInput(context.Background(), 1); e != nil || found {
		t.Fatal("reused ordinary slot inherited the deferred sleep hold", found, e)
	}
}

func TestDeferredEndedDrainCannotReplaceDeliveredWorker(t *testing.T) {
	f := newDeferredProgress(t, "start", agent.AllowAll)
	_, _, response, started, user, _, _ := deferredNative(t)
	f.queueCrash("deferred frozen")
	if e := f.p.closePeer(); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(f.x.lv.dir, host.StatusFile), []byte(`{"exit_code":0}`), 0600); e != nil {
		t.Fatal(e)
	}
	f.append(response, started, user)
	if !f.x.r.step(f.x.lv) || f.state() != "delivered" {
		t.Fatal("fixture did not commit exact final ACK")
	}
	// State-machine seam only, not invented provider output. Prevent a
	// regression from ever reaching a launch, including in a negative test.
	// Force the creating state that makes CannotResume replaceable; a live
	// running-state SessionEnded alone does not exercise that branch.
	if _, e := f.x.r.Store.DB.Exec(`UPDATE sessions SET state='creating' WHERE id=1`); e != nil {
		t.Fatal(e)
	}
	f.x.r.point = func(point string) {
		if point == "after cannot resume" {
			panic("replacement launch attempted during terminal drain")
		}
	}
	defer func() {
		if p := recover(); p != nil {
			t.Fatal(p)
		}
	}()
	lv := f.x.lv
	b := batch{events: []agent.Event{{Kind: agent.SessionEnded, Status: agent.Failed, Reason: agent.CannotResume}}, offset: lv.offset, files: lv.files}
	if _, e := f.x.r.commit(lv, b); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := f.x.r.Store.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); e != nil || n != 2 {
		t.Fatal("terminal drain created a replacement", n, e)
	}
	if f.state() != "delivered" {
		t.Fatal("late terminal event erased exact delivery")
	}
}
