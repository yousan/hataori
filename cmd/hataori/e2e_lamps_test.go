package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #258: lamps on the message that started a
// turn, and one ping to its sender when the turn ends.

// lampsOn returns the statuses set on the message ref, in order.
func lampsOn(d *fakesurface.Driver, ref surface.MessageRef) []surface.Status {
	var out []surface.Status
	for _, c := range d.Calls(ref.Conv) {
		if c.Op == fakesurface.OpSetStatus && c.Ref == ref {
			out = append(out, c.Status)
		}
	}
	return out
}

// pings returns the posts in conv that ping who.
func pings(d *fakesurface.Driver, conv surface.Conversation, who string) []fakesurface.Call {
	var out []fakesurface.Call
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpPost && slices.Contains(c.Message.Notify, who) {
			out = append(out, c)
		}
	}
	return out
}

// anyPings reports whether any post in conv pings anyone.
func anyPings(d *fakesurface.Driver, conv surface.Conversation) bool {
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpPost && len(c.Message.Notify) > 0 {
			return true
		}
	}
	return false
}

func lampsAre(d *fakesurface.Driver, ref surface.MessageRef, want ...surface.Status) func() bool {
	return func() bool { return slices.Equal(lampsOn(d, ref), want) }
}

var (
	working = surface.Working
	done    = surface.Done
	failed  = surface.Failed
)

func TestLampsAndOnePingForACompletedAndAFailedTurn(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	owner := fakesurface.Person(e2eOwner)
	trigger := r.driver.Inject(c1, owner, "!go repo:"+bareRepo(t)+" reply hello")
	th := r.thread(t, 0)
	r.posted(t, th, "hello")
	eventually(t, "working, then done", 15*time.Second, lampsAre(r.driver, trigger.Ref, working, done))
	eventually(t, "the ping", 15*time.Second, func() bool { return len(pings(r.driver, th, e2eOwner)) == 1 })

	failing := r.driver.Inject(th, owner, "fail api_error")
	eventually(t, "working, then error", 15*time.Second, lampsAre(r.driver, failing.Ref, working, failed))
	eventually(t, "the second ping", 15*time.Second, func() bool { return len(pings(r.driver, th, e2eOwner)) == 2 })
	time.Sleep(300 * time.Millisecond)
	if p := pings(r.driver, th, e2eOwner); len(p) != 2 {
		t.Fatalf("%d pings for two turns, want one each", len(p))
	}
	if l := lampsOn(r.driver, trigger.Ref); !slices.Equal(l, []surface.Status{working, done}) {
		t.Fatalf("the first message's lamps %v changed", l)
	}
}

func TestATurnOfAWebhookPostGetsLampsAndPingsNobody(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	trigger := r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply hello")
	th := r.thread(t, 0)
	r.posted(t, th, "hello")
	eventually(t, "working, then done", 15*time.Second, lampsAre(r.driver, trigger.Ref, working, done))
	time.Sleep(300 * time.Millisecond)
	if anyPings(r.driver, th) || anyPings(r.driver, c1) {
		t.Fatalf("a ping for a webhook's turn")
	}
}

func TestLampAndPingAfterARestartDuringATurn(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	trigger := p.driver.Inject(c1, fakesurface.Person(e2eOwner), "!go repo:"+bareRepo(t)+" tool wait 3s\nreply after")
	th := p.thread(t, 0)
	eventually(t, "the tool", 15*time.Second, func() bool { return len(p.events(t, "tool_started", th)) == 1 })
	eventually(t, "working", 15*time.Second, lampsAre(p.driver, trigger.Ref, working))
	p.restart(t)
	p.posted(t, th, "after")
	eventually(t, "done", 15*time.Second, func() bool {
		l := lampsOn(p.driver, trigger.Ref)
		return len(l) > 0 && l[len(l)-1] == done
	})
	eventually(t, "the ping", 15*time.Second, func() bool { return len(pings(p.driver, th, e2eOwner)) >= 1 })
	time.Sleep(500 * time.Millisecond)
	if n := len(pings(p.driver, th, e2eOwner)); n != 1 {
		t.Fatalf("%d pings, want 1", n)
	}
}

func TestEveryReplyInOrderWhenEveryLampFails(t *testing.T) {
	r := newChatRoot(t, nil)
	r.driver.Fail(fakesurface.OpSetStatus, 1<<20, nil)
	startDaemon(t, r.root)
	r.connected(t)
	owner := fakesurface.Person(e2eOwner)
	r.driver.Inject(c1, owner, "!go repo:"+bareRepo(t)+" reply one")
	th := r.thread(t, 0)
	r.posted(t, th, "one")
	for _, w := range []string{"two", "three"} {
		r.driver.Inject(th, owner, "reply "+w)
		r.posted(t, th, w)
	}
	var got []string
	for _, c := range r.driver.Calls(th) {
		if c.Op == fakesurface.OpPost && len(c.Message.Notify) == 0 {
			got = append(got, c.Message.Text)
		}
	}
	if s := strings.Join(got, "|"); s != "reply one|one|two|three" {
		t.Fatalf("replies %q", s)
	}
	for _, c := range append(r.driver.Calls(th), r.driver.Calls(c1)...) {
		if c.Op == fakesurface.OpSetStatus {
			t.Fatalf("a lamp was shown: %+v", c)
		}
	}
}

func TestAMessageFromAGuestGetsNoLamp(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	owner := fakesurface.Person(e2eOwner)
	r.driver.Inject(c1, owner, "!go repo:"+bareRepo(t)+" reply one")
	th := r.thread(t, 0)
	r.posted(t, th, "one")
	guest := r.driver.Inject(th, fakesurface.Person("x-9"), "reply from a guest")
	trigger := r.driver.Inject(c1, fakesurface.Person("x-9"), "!go repo:"+bareRepo(t)+" reply guest thread")
	// A message after them that does get a lamp: once it has, the guest's
	// messages were read.
	next := r.driver.Inject(th, owner, "reply two")
	r.posted(t, th, "two")
	eventually(t, "the owner's lamps", 15*time.Second, lampsAre(r.driver, next.Ref, working, done))
	if l, m := lampsOn(r.driver, guest.Ref), lampsOn(r.driver, trigger.Ref); len(l) != 0 || len(m) != 0 {
		t.Fatalf("a guest's messages got lamps %v and %v", l, m)
	}
}
