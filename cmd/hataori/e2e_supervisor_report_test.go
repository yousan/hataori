package main

import (
	"slices"
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

// postsIn returns the posts in conv, in order.
func postsIn(d *fakesurface.Driver, conv surface.Conversation) []fakesurface.Call {
	var out []fakesurface.Call
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpPost {
			out = append(out, c)
		}
	}
	return out
}

// The acceptance criteria of #651: a wake's turn posts its final answer
// only, and nothing when that answer is "(quiet)"; a person's turn is
// posted whole, "(quiet)" too; "@<name>" of a person who may act in the
// scope mentions them, and nothing else mentions anyone; a worker thread
// is unchanged.
func TestSupervisorWakePostsItsFinalAnswerOnly(t *testing.T) {
	wt := newWakeTest(t, func(cfg map[string]any) {
		cfg["people"] = append(cfg["people"].([]map[string]any),
			map[string]any{"name": "outsider", "ids": []string{fakeSurface + ":out-1"}})
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	person := fakesurface.Person(e2eOwner)
	before := len(postsIn(wt.r.driver, conv))

	// A wake whose final answer is "(quiet)" posts nothing, not even the
	// text before it. Its worker posts as before.
	wt.r.driver.Inject(c1, person, "!go repo:"+wt.repo+" reply reply thinking\nreply reply (quiet)")
	quiet := wt.r.thread(t, 1)
	wt.r.posted(t, quiet, "reply (quiet)")
	wt.r.posted(t, quiet, "reply thinking")
	wt.read(t, wt.ended(t, 2, 1))
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1)
	time.Sleep(300 * time.Millisecond)
	if p := postsIn(wt.r.driver, conv); len(p) != before {
		t.Fatalf("a quiet wake posted %+v", p[before:])
	}

	// A wake with another final answer posts it, and only it; "@<name>"
	// of the owner and of one of the scope's people mentions them, of a
	// person not allowed in the scope and of no person nobody.
	named := "@boss and @dispatch, look; not @outsider, @nobody or x@boss"
	wt.r.driver.Inject(c1, person, "!go repo:"+wt.repo+" reply reply looking\nreply reply "+named)
	loud := wt.r.thread(t, 2)
	wt.r.posted(t, loud, "reply "+named)
	wt.read(t, wt.ended(t, 3, 1))
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 2)
	got := wt.r.posted(t, conv, named)
	if !slices.Equal(got.Message.Notify, []string{e2eOwner, e2eHook}) {
		t.Fatalf("the wake's answer mentions %v, want the owner's and dispatch's ids", got.Message.Notify)
	}
	time.Sleep(300 * time.Millisecond)
	if p := postsIn(wt.r.driver, conv); len(p) != before+1 {
		t.Fatalf("the wake posted %+v, want its final answer only", p[before:])
	}
	for _, c := range append(postsIn(wt.r.driver, quiet), postsIn(wt.r.driver, loud)...) {
		if len(c.Message.Notify) > 0 && c.Message.Text != "Done." {
			t.Fatalf("a worker's post mentions someone: %+v", c)
		}
	}

	// A person's turn is posted whole, "(quiet)" too.
	wt.r.driver.Inject(conv, person, "reply first\nreply (quiet)")
	wt.r.posted(t, conv, "first")
	wt.r.posted(t, conv, "(quiet)")
}
