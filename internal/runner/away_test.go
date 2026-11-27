package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
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
	seedRemote(t, f, 1, "ask colour\ntool wait 30s\nreply after question")
	waitFor(t, "actual waiting worker hook", 5*time.Second, f.has("request", 1))
	c := enableFixtureAway(t, f)
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
		return control.Caller{Principal: old.Principal, From: old.From, Via: old.Via, Scopes: []string{scope}, May: control.OpRead | control.OpSend | control.OpAnswer | control.OpStop}, nil
	}}
	f.run.RevalidatePending = f.core.RevalidatePending
	t.Cleanup(func() { c.connected.Store(true) })
	waitFor(t, "initial current-generation catch-up", 5*time.Second, func() bool {
		a, err := f.run.MachineAvailability(context.Background(), "build")
		return err == nil && a.State == "ready"
	})
	return c
}

func TestAwayResidentRestartDrainsBeforeHeldInputAndStaleStop(t *testing.T) {
	f, _ := remoteFixture(t, false)
	seedRemote(t, f, 1, "tool wait 3s\nreply buffered before reconnect")
	waitFor(t, "original worker turn", 5*time.Second, f.has("turn_started", 1))
	c := enableFixtureAway(t, f)
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
