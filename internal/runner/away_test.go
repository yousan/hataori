package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/workerproto"
)

type fixtureConnection struct {
	connected  atomic.Bool
	generation atomic.Uint64
}

func TestAwayExpiredWorkerHookIsWithdrawnBeforeHeldAnswer(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "short-hook")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	script := "#!/bin/sh\nif [ \"$1\" = hook ]; then\n shift\n exec " + quoted + " hook \"$@\" --timeout 2s\nfi\nexec " + quoted + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f, _ := remoteFixture(t, true, func(c *fake.Config) { c.Hataori = wrapper })
	c := enableFixtureAway(t, f)
	seedRemote(t, f, 1, "ask colour\ntool wait 30s\nreply after question")
	waitFor(t, "actual waiting worker hook", 5*time.Second, f.has("request", 1))
	ctx := context.Background()
	var request string
	if err := f.st.DB.QueryRow(`SELECT handle FROM requests WHERE state='open'`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	c.connected.Store(false)
	if err := owner(f.core).Answer(ctx, control.RequestID(request), control.Answer{Text: "late answer"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2200 * time.Millisecond)
	c.generation.Add(1)
	c.connected.Store(true)
	waitFor(t, "expired hook reconciled before readiness", 8*time.Second, func() bool {
		a, err := f.run.MachineAvailability(ctx, "build")
		return err == nil && a.State == "ready" && counts(t, f.st)["request_withdrawn"] == 1
	})
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	f.st.DB.QueryRow(`SELECT state FROM machine_held_actions WHERE kind='answer'`).Scan(&state)
	if state != "refused" {
		t.Fatalf("expired answer not visibly refused %q", state)
	}
	var replies int
	f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='answer'`).Scan(&replies)
	if replies != 0 {
		t.Fatalf("late hook answer sent %d", replies)
	}
}

func enableFixtureAway(t *testing.T, f *fixture) *fixtureConnection {
	t.Helper()
	c := &fixtureConnection{}
	c.connected.Store(true)
	c.generation.Store(1)
	m := f.run.Remotes["build"]
	open := m.Open
	m.Connection = func() Connection {
		return Connection{Connected: c.connected.Load(), Token: fmt.Sprint(c.generation.Load()), Reason: "fixture transport"}
	}
	m.Open = func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		if !c.connected.Load() {
			return nil, nil, errors.New("isolated fixture connection cut")
		}
		return open(ctx)
	}
	f.run.Remotes["build"] = m
	f.run.AwayEnabled = true
	f.run.AwayGrace = 25 * time.Millisecond
	f.core.Away = &core.MachineAway{Status: func(ctx context.Context, key string) (control.MachineAvailability, error) {
		return f.run.MachineAvailability(ctx, key)
	}, Caller: func(_ context.Context, old control.Caller, scope, repo string) (control.Caller, error) {
		return control.Caller{Principal: old.Principal, From: old.From, Via: old.Via, Scopes: []string{scope}, May: all}, nil
	}}
	f.run.RevalidatePending = f.core.RevalidatePending
	f.run.RevalidateInitial = f.core.RevalidateInitial
	t.Cleanup(func() { c.connected.Store(true) })
	waitFor(t, "initial current-generation catch-up", 5*time.Second, func() bool {
		a, err := f.run.MachineAvailability(context.Background(), "build")
		return err == nil && a.State == "ready"
	})
	return c
}

func TestAwayResidentRestartDrainsBeforeHeldInputAndStaleStop(t *testing.T) {
	f, _ := remoteFixture(t, false)
	c := enableFixtureAway(t, f)
	seedRemote(t, f, 1, "tool wait 3s\nreply buffered before reconnect")
	waitFor(t, "original worker turn", 5*time.Second, f.has("turn_started", 1))
	ctx := context.Background()
	var turn int64
	f.st.DB.QueryRow(`SELECT max(id) FROM turns WHERE session_id=1`).Scan(&turn)
	c.connected.Store(false)
	a, err := f.run.MachineAvailability(ctx, "build")
	if err != nil || a.State == "ready" {
		t.Fatalf("cut did not block immediately %+v %v", a, err)
	}
	if err := owner(f.core).Stop(ctx, 1, turn); !errors.Is(err, core.ErrActionHeld) {
		t.Fatalf("stop did not report pending %v", err)
	}
	if err := owner(f.core).Send(ctx, 1, "held-after-cut", control.Input{Text: "reply held after reconnect"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3300 * time.Millisecond)
	a, err = f.run.MachineAvailability(ctx, "build")
	if err != nil || a.State != "away" {
		t.Fatalf("grace %+v %v", a, err)
	}
	if got := counts(t, f.st)["turn_ended"]; got != 0 {
		t.Fatalf("cut was interpreted as an end %d", got)
	}
	f.run.Close()
	next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 20 * time.Millisecond, Remotes: f.run.Remotes, AwayEnabled: true, AwayGrace: 25 * time.Millisecond, RevalidatePending: f.core.RevalidatePending}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(t, f.st)["turn_started"]; got != 1 {
		t.Fatalf("restart dispatched while away %d", got)
	}
	c.generation.Add(1)
	c.connected.Store(true)
	// Even a restored transport cannot dispatch before the follower commits
	// the original end and resolves its receipt journal.
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retained output drained before readiness", 8*time.Second, func() bool {
		a, err := next.MachineAvailability(ctx, "build")
		return err == nil && a.State == "ready" && counts(t, f.st)["turn_ended"] == 1
	})
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "held input finishes once", 8*time.Second, f.has("turn_ended", 2))
	got := counts(t, f.st)
	if got["turn_started"] != 2 || got["turn_ended"] != 2 || got["stop"] != 0 {
		t.Fatalf("stale stop or duplicate lifecycle %v", got)
	}
	var stopState string
	f.st.DB.QueryRow(`SELECT state FROM machine_held_actions WHERE kind='stop'`).Scan(&stopState)
	if stopState != "refused" {
		t.Fatalf("stale stop not reported %q", stopState)
	}
	if err := owner(f.core).Send(ctx, 1, "held-after-cut", control.Input{Text: "reply held after reconnect"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	if counts(t, f.st)["turn_started"] != 2 {
		t.Fatal("repeated held input started another turn")
	}
	var buffered, dispatched uint64
	f.st.DB.QueryRow(`SELECT max(pos) FROM events WHERE kind='turn_ended' AND json_extract(data,'$.turn')=?`, turn).Scan(&buffered)
	f.st.DB.QueryRow(`SELECT max(pos) FROM events WHERE kind='machine_action' AND json_extract(data,'$.action')='send' AND json_extract(data,'$.state')='dispatched'`).Scan(&dispatched)
	if buffered == 0 || dispatched <= buffered {
		t.Fatalf("held dispatch preceded retained end %d %d", buffered, dispatched)
	}
}

// This fixture owns no processes. Rebuilding its codec emulates both a
// reconnect and a main restart from durable write/pause records.
func TestAwayWrittenDeliveryBudgetSurvivesCodecRebuild(t *testing.T) {
	f := placementRecordFixture(t)
	ctx := context.Background()
	if err := f.st.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at := now.Add(-8 * time.Second)
	first, _ := json.Marshal(core.FirstInput{Text: "unreported", Sent: true, Written: true, Current: true, WrittenAt: at.UnixMilli()})
	for _, v := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','budget','owner','running',1,1,'build')`, nil},
		{`INSERT INTO sessions(id,thread_id,agent,state,first_input,spec,created_at,updated_at,launch) VALUES(1,1,'fake','running',?,'{}',1,1,0)`, []any{string(first)}},
		{`INSERT INTO session_pauses(session_id,began_at,ended_at) VALUES(1,?,?)`, []any{at.Add(time.Second).UnixMilli(), at.Add(5 * time.Second).UnixMilli()}},
	} {
		if _, err := f.st.DB.Exec(v.q, v.args...); err != nil {
			t.Fatal(err)
		}
	}
	f.run.AwayEnabled = true
	ad := f.run.Adapters["fake"]
	for i := 0; i < 2; i++ {
		lv := &live{id: 1, remote: true, codec: ad.NewCodec(agent.Spec{})}
		f.run.handOverWritten(lv)
		clock := f.run.codecNow(lv)
		if a := lv.codec.Tick(clock); len(a.Events) != 0 {
			t.Fatalf("expired before remaining second: %+v", a.Events)
		}
		a := lv.codec.Tick(clock.Add(1500 * time.Millisecond))
		if len(a.Events) != 1 || a.Events[0].Kind != agent.TurnEnded || a.Events[0].Reason != agent.NotDelivered {
			t.Fatalf("rebuild reset delivery budget: %+v", a.Events)
		}
	}
}

type awayInterruptCodec struct{ agent.Codec }

func (c awayInterruptCodec) Interrupt() agent.Actions {
	return agent.Actions{Write: [][]byte{[]byte("interrupt")}}
}

func TestAwayReadinessCutAtWriteAndStopBoundary(t *testing.T) {
	for _, kind := range []string{"input", "stop"} {
		t.Run(kind, func(t *testing.T) {
			f := placementRecordFixture(t)
			ctx := context.Background()
			if err := f.st.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(core.FirstInput{Text: "unsent", Sent: true})
			for _, v := range []struct {
				q    string
				args []any
			}{
				{`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','boundary','owner','running',1,1,'build')`, nil},
				{`INSERT INTO sessions(id,thread_id,agent,state,first_input,spec,created_at,updated_at,launch) VALUES(1,1,'fake','running',?,'',1,1,0)`, []any{string(raw)}},
				{`INSERT INTO turns(id,session_id,started_by,principal,started_at) VALUES(1,1,'person','owner',1)`, nil},
			} {
				if _, err := f.st.DB.Exec(v.q, v.args...); err != nil {
					t.Fatal(err)
				}
			}
			connected := true
			checks := 0
			f.run.Remotes = map[string]RemoteMachine{"build": {Connection: func() Connection {
				checks++
				if kind == "stop" && checks == 2 {
					connected = false
				}
				return Connection{Connected: connected, Token: "boundary"}
			}}}
			if _, err := f.st.ObserveConnection(ctx, "build", true, "boundary", "", false, time.Second, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := f.st.FinishCatchup(ctx, "build", "boundary", time.Now()); err != nil {
				t.Fatal(err)
			}
			f.run.AwayEnabled = true
			lv := &live{id: 1, remote: true, codec: awayInterruptCodec{f.run.Adapters["fake"].NewCodec(agent.Spec{})}}
			f.run.live = map[int64]*live{1: lv}
			t.Cleanup(func() { f.run.live = nil })
			f.run.point = func(p string) {
				if p == "input marked" {
					connected = false
				}
			}
			var err error
			if kind == "input" {
				err = f.run.act(lv, agent.Actions{Inputs: []string{firstInputID}, Write: [][]byte{[]byte("unsent")}})
			} else {
				err = f.run.Interrupt(ctx, 1, 1, session.Stopper{Principal: "owner"})
			}
			if !errors.Is(err, core.ErrMachineUnchecked) {
				t.Fatalf("readiness refusal hidden: %v", err)
			}
			var first, stopped string
			f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=1`).Scan(&first)
			var in core.FirstInput
			if err := json.Unmarshal([]byte(first), &in); err != nil {
				t.Fatal(err)
			}
			if in.Written {
				t.Fatal("definitely-unsent input retained written marker")
			}
			f.st.DB.QueryRow(`SELECT stopped_by FROM turns WHERE id=1`).Scan(&stopped)
			if stopped != "" {
				t.Fatalf("unsent stop claimed success: %s", stopped)
			}
			if n := counts(t, f.st)["machine_action"]; n != 0 {
				t.Fatalf("unwritten effect counted: %d", n)
			}
		})
	}
}

// Missing retained output is deliberately different from a verified gone
// inventory entry. No process exists or is inspected in this record fixture.
type awayLostHistory struct{ machineio.Session }

func (awayLostHistory) Source(context.Context, string) (machineio.Source, error) {
	return machineio.Source{}, errors.New("isolated retained history unavailable")
}
func (awayLostHistory) Waiting(context.Context) ([]host.WaitingCall, error) { return nil, nil }

func TestAwayUnknownInventoryAndConfirmedGoneDuringWarmReplay(t *testing.T) {
	f := placementRecordFixture(t)
	ctx := context.Background()
	if err := f.st.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','reboot','owner','running',1,1,'build')`,
		`INSERT INTO sessions(id,thread_id,agent,state,spec,created_at,updated_at,launch) VALUES(1,1,'fake','running','{}',1,1,0)`,
		`INSERT INTO turns(id,session_id,started_by,principal,started_at) VALUES(1,1,'person','owner',1)`,
		`INSERT INTO session_machine_runs(session_id,machine,run,workspace,agent_config,phase) VALUES(1,'build','original-run','handle','default','running')`,
		`INSERT INTO machine_reservations(thread_id,machine,agent,operation,created_at) VALUES(1,'build','fake','original',1)`,
		`INSERT INTO machine_inventory(machine,thread_id,session,run,state) VALUES('build',1,'s-1','original-run','unknown')`,
	} {
		if _, err := f.st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.run.AwayEnabled = true
	f.run.Remotes = map[string]RemoteMachine{"build": {Connection: func() Connection { return Connection{Connected: true, Token: "verified-reboot"} }}}
	ad := f.run.Adapters["fake"]
	lv := &live{id: 1, dir: StateDir(f.root, 1), remote: true, adapter: ad, codec: ad.NewCodec(agent.Spec{}), files: map[string]int64{}, io: awayLostHistory{}, sources: map[string]machineio.Source{}, warmPending: true, offset: 1}
	if !f.run.step(lv) || counts(t, f.st)["turn_ended"] != 0 {
		t.Fatal("unknown inventory was interpreted as reboot")
	}
	var slots int
	f.st.DB.QueryRow(`SELECT count(*) FROM machine_reservations`).Scan(&slots)
	if slots != 1 {
		t.Fatal("unknown host released its reservation")
	}
	if _, err := f.st.DB.Exec(`UPDATE machine_inventory SET state='gone'`); err != nil {
		t.Fatal(err)
	}
	if f.run.step(lv) {
		t.Fatal("confirmed gone warm replay did not terminate recovery")
	}
	var status, reason, machine string
	f.st.DB.QueryRow(`SELECT status,reason FROM turns WHERE id=1`).Scan(&status, &reason)
	f.st.DB.QueryRow(`SELECT machine FROM threads WHERE id=1`).Scan(&machine)
	if status != "failure" || reason != "interrupted" || machine != "build" || counts(t, f.st)["turn_ended"] != 1 {
		t.Fatalf("confirmed reboot recovery %s %s %s %v", status, reason, machine, counts(t, f.st))
	}
	f.run.step(lv)
	if counts(t, f.st)["turn_ended"] != 1 {
		t.Fatal("repeated gone observation ended the turn twice")
	}
}

func TestAwayRevokedPendingInputPreservesEventSequence(t *testing.T) {
	f := placementRecordFixture(t)
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','sequence','owner','running',1,1,'local')`,
		`INSERT INTO sessions(id,thread_id,agent,state,last_seq,spec,created_at,updated_at,launch) VALUES(1,1,'fake','running',5,'',1,1,0)`,
		`INSERT INTO turns(id,session_id,started_by,principal,started_at) VALUES(1,1,'person','owner',1)`,
		`INSERT INTO pending_inputs(id,session_id,request,data,context) VALUES(1,1,'','{"text":"revoked","origin":{"turn":1,"principal":"owner"}}','')`,
	} {
		if _, err := f.st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.run.RevalidatePending = func(context.Context, int64, int64) error { return core.ErrForbidden }
	lv := &live{id: 1, seq: 5, files: map[string]int64{}}
	called := false
	err := f.run.handOver(lv, 1, false, func() (agent.Actions, error) { called = true; return agent.Actions{}, nil })
	if !errors.Is(err, core.ErrForbidden) || called || lv.seq != 6 {
		t.Fatalf("refusal sequence or delivery: %v %v %d", err, called, lv.seq)
	}
	if _, err := f.run.commit(lv, batch{events: []agent.Event{{Kind: agent.TurnStarted, Input: "autonomous", StartedBy: agent.ByTerminal}}}); err != nil {
		t.Fatal(err)
	}
	if lv.seq != 7 || counts(t, f.st)["turn_started"] != 1 {
		t.Fatalf("next real event dropped: %d %v", lv.seq, counts(t, f.st))
	}
}

func TestAwayAcceptedAnswerRefusalIsDurable(t *testing.T) {
	for _, outcome := range []string{"codec expired", "complete receipt", "refused receipt"} {
		t.Run(outcome, func(t *testing.T) {
			f := placementRecordFixture(t)
			for _, q := range []string{
				`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','answer-refusal','owner','running',1,1,'local')`,
				`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(1,1,'fake','running',1,1)`,
				`INSERT INTO machine_held_actions(id,thread_id,session_id,run,turn,request,kind,key,data,state,created_at) VALUES(1,1,1,'run',1,'q1','answer','q1','{}','dispatched',1)`,
				`INSERT INTO pending_inputs(id,session_id,request,data,context,held_action) VALUES(1,1,'q1','{}','',1)`,
			} {
				if _, err := f.st.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			lv := &live{id: 1}
			if outcome == "codec expired" {
				if err := f.run.handOver(lv, 1, true, func() (agent.Actions, error) { return agent.Actions{}, errors.New("original request expired") }); err == nil {
					t.Fatal("expired request unexpectedly accepted")
				}
			} else {
				state := "complete"
				if outcome == "refused receipt" {
					state = "refused"
				}
				if err := f.run.resolveActionInputs(lv, `["answer:1"]`, state); err != nil {
					t.Fatal(err)
				}
			}
			var pending int
			var state string
			f.st.DB.QueryRow(`SELECT count(*) FROM pending_inputs`).Scan(&pending)
			f.st.DB.QueryRow(`SELECT state FROM machine_held_actions WHERE id=1`).Scan(&state)
			want := "refused"
			if outcome == "complete receipt" {
				want = "dispatched"
			}
			if pending != 0 || state != want {
				t.Fatalf("terminal answer outcome lost: %d %s", pending, state)
			}
			if want == "refused" && counts(t, f.st)["machine_action"] != 1 {
				t.Fatal("accepted answer refusal was invisible")
			}
		})
	}
}

func TestAwayHeldAnswerSurvivesClosedMainDatabase(t *testing.T) {
	var reopened *store.Store
	// Register first so the resident's own-host cleanup runs while the new DB
	// is still open; no synthetic PID cleanup or process-name signalling.
	t.Cleanup(func() {
		if reopened != nil {
			reopened.Close()
		}
	})
	f, _ := remoteFixture(t, true)
	c := enableFixtureAway(t, f)
	seedRemote(t, f, 1, "ask colour\nreply resumed answer")
	waitFor(t, "worker question before cut", 5*time.Second, f.has("request", 1))
	ctx := context.Background()
	var request string
	if err := f.st.DB.QueryRow(`SELECT handle FROM requests WHERE state='open'`).Scan(&request); err != nil {
		t.Fatal(err)
	}
	c.connected.Store(false)
	for i := 0; i < 2; i++ {
		if err := owner(f.core).Answer(ctx, control.RequestID(request), control.Answer{Text: "red"}); err != nil {
			t.Fatal(err)
		}
	}
	f.run.Close()
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	reopened, err = store.Open(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.st = reopened
	f.core.Store = reopened
	f.core.Machine.Store = reopened
	next := &Runner{Store: reopened, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: 20 * time.Millisecond, Remotes: f.run.Remotes, AwayEnabled: true, AwayGrace: 25 * time.Millisecond, RevalidatePending: f.core.RevalidatePending, RevalidateInitial: f.core.RevalidateInitial}
	f.run = next
	f.core.Runner = next
	if err := next.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	var replies int
	f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='answer'`).Scan(&replies)
	if replies != 0 {
		t.Fatal("answer was sent while away")
	}
	c.generation.Add(1)
	c.connected.Store(true)
	waitFor(t, "restarted main caught up", 8*time.Second, func() bool { a, e := next.MachineAvailability(ctx, "build"); return e == nil && a.State == "ready" })
	if err := f.core.DispatchHeld(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "held answer completed once", 8*time.Second, f.has("turn_ended", 1))
	var held, notices int
	f.st.DB.QueryRow(`SELECT count(*) FROM machine_held_actions`).Scan(&held)
	f.st.DB.QueryRow(`SELECT count(*) FROM machine_absence_notices`).Scan(&notices)
	f.st.DB.QueryRow(`SELECT count(*) FROM session_machine_actions WHERE operation='answer' AND state='complete'`).Scan(&replies)
	if held != 1 || notices != 1 || replies != 1 {
		t.Fatalf("answer/notice duplicated across DB restart: %d %d %d", held, notices, replies)
	}
}
