package main

import (
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #639: in a supervisor's thread a completed
// turn that a person's message started gets its lamps and no ping; a
// failed turn, and one that ends waiting on an answer, ping that person
// once. A worker thread of the same scope still pings when its turn
// completes.
func TestSupervisorThreadPingsOnlyWhenAPersonIsNeeded(t *testing.T) {
	r, _ := svRoot(t, true)
	stop := runDaemon(t, r.root)
	defer stop()
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sv := supervisorThread(t, c, "c1", control.Idle)
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	owner := fakesurface.Person(e2eOwner)

	asked := r.driver.Inject(conv, owner, "reply ready to help")
	r.posted(t, conv, "ready to help")
	eventually(t, "working, then done", 15*time.Second, lampsAre(r.driver, asked.Ref, working, done))
	time.Sleep(300 * time.Millisecond)
	if p := pings(r.driver, conv, e2eOwner); len(p) != 0 {
		t.Fatalf("a completed turn in the supervisor's thread pinged: %+v", p)
	}

	failing := r.driver.Inject(conv, owner, "fail api_error")
	eventually(t, "working, then error", 15*time.Second, lampsAre(r.driver, failing.Ref, working, failed))
	eventually(t, "the failed turn's ping", 15*time.Second, func() bool { return len(pings(r.driver, conv, e2eOwner)) == 1 })

	question := r.driver.Inject(conv, owner, "ask "+colours)
	eventually(t, "working, then waiting", 15*time.Second, lampsAre(r.driver, question.Ref, working, surface.Waiting))
	eventually(t, "the waiting turn's ping", 15*time.Second, func() bool { return len(pings(r.driver, conv, e2eOwner)) == 2 })
	time.Sleep(300 * time.Millisecond)
	p := pings(r.driver, conv, e2eOwner)
	if len(p) != 2 || p[0].Message.Text != "The turn failed." || p[1].Message.Text != "Waiting for your answer." {
		t.Fatalf("pings in the supervisor's thread %+v, want one for the failed turn and one for the question", p)
	}

	// A worker thread of the same scope is unchanged.
	trigger := r.driver.Inject(c1, owner, "!go repo:"+bareRepo(t)+" reply hello")
	th := r.thread(t, 1)
	r.posted(t, th, "hello")
	eventually(t, "the worker's lamps", 15*time.Second, lampsAre(r.driver, trigger.Ref, working, done))
	eventually(t, "the worker's ping", 15*time.Second, func() bool { return len(pings(r.driver, th, e2eOwner)) == 1 })
	if n := len(pings(r.driver, conv, e2eOwner)); n != 2 {
		t.Fatalf("%d pings in the supervisor's thread after the worker's turn, want 2", n)
	}
}
