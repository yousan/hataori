package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #542: a turn a person stopped is shown as
// stopped, not as a failure.

// stoppedRoot is a chat root whose owner is the person ada, with commands.
func stoppedRoot(t *testing.T) *chatRoot {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}}}
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": fakeSurface, "enabled": true}}
	})
	l := fakesurface.DefaultLimits
	l.Commands = true
	r.driver.SetLimits(l)
	return r
}

// stoppedShown checks how a stopped turn is shown: one line naming ada,
// the message that started the turn back to no lamp after working, no
// failure line and no ping beyond the earlier ones.
func stoppedShown(t *testing.T, r *chatRoot, th surface.Conversation, msg surface.MessageRef, pingsBefore int) {
	t.Helper()
	r.posted(t, th, "Stopped by ada.")
	eventually(t, "working, then no lamp", 15*time.Second, lampsAre(r.driver, msg, working, surface.NoStatus))
	time.Sleep(300 * time.Millisecond)
	if n := posts(r.driver, th, "Stopped by ada."); n != 1 {
		t.Fatalf("%d stopped lines, want 1", n)
	}
	for _, c := range r.driver.Calls(th) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "failed") {
			t.Fatalf("the thread shows %q", c.Message.Text)
		}
	}
	if l := lampsOn(r.driver, msg); slices.Contains(l, failed) {
		t.Fatalf("lamps %v", l)
	}
	if p := pings(r.driver, th, e2eOwner); len(p) != pingsBefore {
		t.Fatalf("%d pings, want the %d before the stop", len(p), pingsBefore)
	}
}

// A turn stopped by /stop, and a turn after it that fails for another
// reason: the first is shown as stopped by ada, the second still gets the
// error lamp and one ping.
func TestATurnStoppedByTheStopCommandIsShownAsStopped(t *testing.T) {
	r := stoppedRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	owner := fakesurface.Person(e2eOwner)
	r.driver.Inject(c1, owner, "!go repo:"+bareRepo(t)+" reply hello")
	th := r.thread(t, 0)
	r.posted(t, th, "hello")
	eventually(t, "the first ping", 15*time.Second, func() bool { return len(pings(r.driver, th, e2eOwner)) == 1 })

	long := r.driver.Inject(th, owner, "tool wait 60s\nreply never")
	eventually(t, "the tool", 15*time.Second, func() bool { return len(r.events(t, "tool_started", th)) == 1 })
	r.driver.InjectCommand(th, owner, "stop")
	if e := r.turnEnds(t, th, 2, 10*time.Second); e["outcome"] != "failure" || e["reason"] != "interrupted" {
		t.Fatalf("the turn ended %v; the event keeps failure, interrupted", e)
	}
	stoppedShown(t, r, th, long.Ref, 1)

	failing := r.driver.Inject(th, owner, "fail api_error")
	eventually(t, "working, then error", 15*time.Second, lampsAre(r.driver, failing.Ref, working, failed))
	eventually(t, "the ping of the failure", 15*time.Second, func() bool { return len(pings(r.driver, th, e2eOwner)) == 2 })
	r.posted(t, th, "The turn failed: error (api_error).")
}

// The Stop button, on a turn whose agent does not end it in time: its
// session is stopped (reason stopped), and the turn is shown as stopped
// by ada all the same.
func TestATurnStoppedByTheStopButtonIsShownAsStopped(t *testing.T) {
	quickLines(t)
	r := stoppedRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	long := r.driver.Inject(c1, fakesurface.Person(e2eOwner), "!go repo:"+bareRepo(t)+" interrupt-delay 60s\ntool wait 60s\nreply never")
	th := r.thread(t, 0)
	line, token := stopLine(t, r.driver, th)
	if err := r.driver.Press(line.Ref, e2eOwner, token); err != nil {
		t.Fatal(err)
	}
	if e := r.turnEnds(t, th, 1, 15*time.Second); e["outcome"] != "failure" || e["reason"] != "stopped" {
		t.Fatalf("the turn ended %v; the event keeps failure, stopped", e)
	}
	stoppedShown(t, r, th, long.Ref, 0)
}

// hataori stop: the owner's command, so the turn is shown as stopped by
// the owner.
func TestATurnStoppedByHataoriStopIsShownAsStopped(t *testing.T) {
	r := stoppedRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	long := r.driver.Inject(c1, fakesurface.Person(e2eOwner), "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
	th := r.thread(t, 0)
	eventually(t, "the tool", 15*time.Second, func() bool { return len(r.events(t, "tool_started", th)) == 1 })
	if code, _, errOut := cli(t, "stop", "--root", r.root, numberOf(t, r.root, th)); code != 0 {
		t.Fatalf("hataori stop: %d %s", code, errOut)
	}
	if e := r.turnEnds(t, th, 1, 10*time.Second); e["outcome"] != "failure" || e["reason"] != "interrupted" {
		t.Fatalf("the turn ended %v", e)
	}
	stoppedShown(t, r, th, long.Ref, 0)
}
