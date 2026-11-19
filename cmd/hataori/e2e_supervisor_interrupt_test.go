package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

type interruptFixture struct {
	root   *chatRoot
	stop   func()
	log    func() string
	owner  *wire.Client
	sv     control.ThreadInfo
	worker control.ThreadID
	m      *supervisor.MCP
	token  string
	conv   surface.Conversation
}

func newInterruptFixture(t *testing.T, duration string) *interruptFixture {
	t.Helper()
	r, _ := svRoot(t, true, func(cfg map[string]any) {
		people := cfg["people"].([]map[string]any)
		people[2]["ids"] = []string{supervisor.ID("c1"), fakeSurface + ":sv-person"}
	})
	f := &interruptFixture{root: r}
	f.stop, f.log = runDaemonLog(t, r.root)
	t.Cleanup(func() {
		if f.owner != nil {
			f.owner.Close()
		}
		f.stop()
	})
	r.connected(t)
	var err error
	f.owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	f.sv = supervisorThread(t, f.owner, "c1", control.Idle)
	f.conv = surface.Conversation{Channel: f.sv.Links[0].Channel, Thread: f.sv.Links[0].Thread}
	var sid int64
	if err := dbOf(t, r.root).QueryRow(`SELECT id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, f.sv.ID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(runner.StateDir(r.root, sid), supervisor.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	f.token = strings.TrimSpace(string(token))
	f.m = &supervisor.MCP{Dial: func() (control.API, error) { return wire.DialToken(r.root, f.token) }, Scope: "c1"}
	f.worker, err = f.owner.Start(context.Background(), control.StartRequest{Key: "worker", Scope: "c1", Thread: "worker", Repo: bareRepo(t), Input: control.Input{Text: "tool t " + duration}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", 15*time.Second, func() bool {
		d, e := f.owner.Thread(context.Background(), f.worker, 0, 0)
		return e == nil && d.State == control.Running
	})
	return f
}

func (f *interruptFixture) ask(t *testing.T) fakesurface.Call {
	t.Helper()
	out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(f.worker), "reason": "urgent"})
	if out.IsError || !strings.HasPrefix(out.Text, "asked a person") {
		t.Fatalf("interrupt tool: %+v", out)
	}
	var q fakesurface.Call
	eventually(t, "approval buttons", 15*time.Second, func() bool {
		for _, c := range f.root.driver.Calls(f.conv) {
			if c.Op == fakesurface.OpPost && strings.HasPrefix(c.Message.Text, "Interrupt thread ") && len(c.Message.Choices) == 2 {
				q = c
				return true
			}
		}
		return false
	})
	return q
}
func (f *interruptFixture) press(t *testing.T, q fakesurface.Call, who, label string) {
	t.Helper()
	if err := f.root.driver.PressMany(q.Ref, who, choiceToken(t, q, label)); err != nil {
		t.Fatal(err)
	}
}
func (f *interruptFixture) outcome(t *testing.T, want string) {
	t.Helper()
	eventually(t, "approval outcome "+want, 15*time.Second, func() bool {
		ev := threadEvents(t, f.owner, f.sv.ID, "approval_answered")
		return len(ev) == 1 && ev[0]["outcome"] == want
	})
}
func (f *interruptFixture) requests(t *testing.T) []control.Request {
	t.Helper()
	d, e := f.owner.Thread(context.Background(), f.sv.ID, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	return d.Requests
}
func (f *interruptFixture) restart(t *testing.T) {
	t.Helper()
	f.owner.Close()
	f.stop()
	f.stop, f.log = runDaemonLog(t, f.root.root)
	f.root.connected(t)
	var err error
	f.owner, err = wire.Dial(f.root.root)
	if err != nil {
		t.Fatal(err)
	}
	f.m = &supervisor.MCP{Dial: func() (control.API, error) { return wire.DialToken(f.root.root, f.token) }, Scope: "c1"}
}

func TestInterruptAsksAPerson(t *testing.T) {
	f := newInterruptFixture(t, "30s")
	f.ask(t)
	d, err := f.owner.Thread(context.Background(), f.worker, 0, 0)
	if err != nil || d.State != control.Running || len(threadEvents(t, f.owner, f.worker, "turn_ended")) != 0 {
		t.Fatalf("worker: %+v %v", d, err)
	}
	requests := f.requests(t)
	if len(requests) != 1 || requests[0].Kind != control.Approval || len(threadEvents(t, f.owner, f.sv.ID, "approval_asked")) != 1 {
		t.Fatalf("requests: %+v", requests)
	}
	out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(f.sv.ID), "reason": "idle"})
	if !out.IsError || !strings.HasPrefix(out.Text, "not busy") {
		t.Fatalf("idle: %+v", out)
	}
	if len(threadEvents(t, f.owner, f.sv.ID, "approval_asked")) != 1 {
		t.Fatal("idle request stored")
	}
}
func TestInterruptApproved(t *testing.T) {
	f := newInterruptFixture(t, "30s")
	q := f.ask(t)
	f.press(t, q, e2eOwner, "Approve")
	f.outcome(t, "interrupted")
	eventually(t, "interrupted turn", 15*time.Second, func() bool {
		for _, e := range threadEvents(t, f.owner, f.worker, "turn_ended") {
			if e["reason"] == "interrupted" {
				return true
			}
		}
		return false
	})
	eventually(t, "marked input", 15*time.Second, func() bool {
		for _, in := range threadEvents(t, f.owner, f.worker, "input") {
			if in["text"] == supervisor.Mark+"\nurgent" {
				return true
			}
		}
		return false
	})
	f.root.posted(t, f.conv, fmt.Sprintf("Approved by boss: thread %d's turn was interrupted, and it got the reason.", f.worker))
	f.press(t, q, e2eOwner, "Approve")
	time.Sleep(200 * time.Millisecond)
	if len(threadEvents(t, f.owner, f.sv.ID, "approval_answered")) != 1 || len(threadEvents(t, f.owner, f.worker, "input")) != 2 {
		t.Fatal("duplicate approval changed the worker")
	}
}
func TestInterruptRefused(t *testing.T) {
	f := newInterruptFixture(t, "30s")
	q := f.ask(t)
	for i, who := range []string{"guest", "sv-person"} {
		f.press(t, q, who, "Approve")
		want := i + 1
		eventually(t, "refusal logged", 5*time.Second, func() bool { return strings.Count(f.log(), "control call refused") >= want })
		if len(f.requests(t)) != 1 || len(threadEvents(t, f.owner, f.worker, "input")) != 1 {
			t.Fatal("unauthorized approval changed worker")
		}
	}
	sv, err := wire.DialToken(f.root.root, f.token)
	if err != nil {
		t.Fatal(err)
	}
	defer sv.Close()
	if err := sv.Answer(context.Background(), f.requests(t)[0].ID, control.Answer{Allow: true}); err == nil {
		t.Fatal("supervisor answered")
	}
	eventually(t, "refusals logged", 5*time.Second, func() bool {
		return refusedLine(f.log(), "Answer", "c1") && strings.Count(f.log(), "control call refused") >= 3
	})
	if len(f.requests(t)) != 1 || len(threadEvents(t, f.owner, f.worker, "input")) != 1 {
		t.Fatal("unauthorized approval changed worker")
	}
	f.press(t, q, e2eOwner, "Refuse")
	f.outcome(t, "refused")
	f.root.posted(t, f.conv, fmt.Sprintf("Refused by boss: thread %d was not interrupted.", f.worker))
	d, _ := f.owner.Thread(context.Background(), f.worker, 0, 0)
	if d.State != control.Running {
		t.Fatalf("worker: %s", d.State)
	}
}
func TestInterruptApprovedAfterARestart(t *testing.T) {
	f := newInterruptFixture(t, "30s")
	q := f.ask(t)
	f.restart(t)
	out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(f.worker), "reason": "again"})
	if out.IsError || !strings.HasPrefix(out.Text, "asked a person") {
		t.Fatalf("restored token: %+v", out)
	}
	f.press(t, q, e2eOwner, "Approve")
	f.outcome(t, "interrupted")
}
func TestInterruptApprovedAfterTheTurnEnded(t *testing.T) {
	f := newInterruptFixture(t, "3s")
	q := f.ask(t)
	eventually(t, "worker finished", 15*time.Second, func() bool { return len(threadEvents(t, f.owner, f.worker, "turn_ended")) == 1 })
	f.press(t, q, e2eOwner, "Approve")
	f.outcome(t, "ended")
	if len(threadEvents(t, f.owner, f.worker, "input")) != 1 || len(threadEvents(t, f.owner, f.worker, "turn_started")) != 1 {
		t.Fatal("input or turn was started after turn ended")
	}
	f.root.posted(t, f.conv, fmt.Sprintf("Approved by boss, but thread %d's turn had already ended: nothing was interrupted.", f.worker))
}
