package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// atOnce has n of scope c1's conversations run a turn at a time.
func atOnce(n int) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["conversations_at_once"] = n
	}
}

// call starts the conversation key with script, or finds it.
func (f *svTalks) call(t *testing.T, key, script string) control.ThreadID {
	t.Helper()
	id, err := f.as(people.Join(fakeSurface, svCaller), key, script)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// long starts the conversation key, whose turn runs for a while, and waits
// until it runs.
func (f *svTalks) long(t *testing.T, key string) control.ThreadID {
	t.Helper()
	id := f.call(t, key, "tool t 100s")
	f.busy(t, id)
	return id
}

// says posts text in the chat thread t-<key>, as a person of the scope.
func (f *svTalks) says(key, text string) {
	f.wt.r.driver.Inject(f.chat(key), fakesurface.Person(svCaller), text)
}

// waitsFor is the line of a message that waits for the conversations that
// talk in the chat threads keys.
func (f *svTalks) waitsFor(t *testing.T, keys ...string) string {
	t.Helper()
	var running []string
	for _, k := range keys {
		running = append(running, fmt.Sprintf("thread %s (%s)", numberOf(t, f.wt.r.root, f.chat(k)), f.wt.r.driver.Link(f.chat(k))))
	}
	return "This waits until another conversation with the supervisor ends its turn. Running: " + strings.Join(running, ", ") + "."
}

// waitLines counts the lines posted in conv of messages that wait.
func (f *svTalks) waitLines(conv surface.Conversation) int {
	n := 0
	for _, c := range f.wt.r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && strings.HasPrefix(c.Message.Text, "This waits ") {
			n++
		}
	}
	return n
}

// With as many conversations running as may, one more call gets the line
// and starts no turn, also when it is made again and after a restart; it
// runs, once, when one of them ends. Meanwhile an order, a worker's input
// from chat and from outside, the own thread's wake and a person's message
// to it are given at once (#875).
func TestConversationsAtOnce(t *testing.T) {
	f := newSvTalks(t, "act", atOnce(2))
	f.wt.r.deadline = time.Now().Add(120 * time.Second)
	ctx := context.Background()
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	a, b := f.long(t, "a"), f.long(t, "b")
	c := f.call(t, "c", "reply hello from c")
	line := f.waitsFor(t, "a", "b")
	f.wt.r.posted(t, f.chat("c"), line)
	waits := func(when string) {
		t.Helper()
		d, err := f.owner.Thread(ctx, c, 0, 0)
		if err != nil || d.State != control.Asleep || len(f.wt.inputs(t, c)) != 0 || f.wt.turns(t, c) != 0 || f.waitLines(f.chat("c")) != 1 {
			t.Fatalf("%s: the call that waits is %s (%v), with the inputs %q, %d turns and %d lines; want nothing started and one line", when,
				d.State, err, f.wt.inputs(t, c), f.wt.turns(t, c), f.waitLines(f.chat("c")))
		}
	}
	waits("at once")

	f.wt.worker(t, f.owner, "c1", "order", "reply ordered")
	f.says("w", "reply from chat")
	f.wt.ended(t, w, 2)
	if err := f.owner.Send(ctx, w, "outside", control.Input{Text: "reply from outside"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	f.wt.read(t, f.wt.ended(t, w, 3))
	f.wt.quiet(t)
	f.wt.woken(t, f.owner, f.sv.ID, 1)
	own := f.wt.turns(t, f.sv.ID)
	f.wt.r.driver.Inject(f.conv, fakesurface.Person(svCaller), "reply to a person")
	f.wt.ended(t, f.sv.ID, own+1)
	waits("after the others' turns")

	if got := f.call(t, "c", "reply hello from c"); got != c {
		t.Fatalf("the call again: thread %d, want %d", got, c)
	}
	waits("called again")
	f.restart(t)
	if got := f.call(t, "c", "reply hello from c"); got != c {
		t.Fatalf("the call again after a restart: thread %d, want %d", got, c)
	}
	waits("after a restart")

	if err := f.owner.Stop(ctx, a, 0); err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, c, 1)
	f.wt.r.postedWith(t, f.chat("c"), "hello from c")
	f.restart(t)
	time.Sleep(300 * time.Millisecond)
	if in := f.wt.inputs(t, c); len(in) != 1 || f.wt.turns(t, c) != 1 || f.waitLines(f.chat("c")) != 1 {
		t.Fatalf("it ran with %q, %d turns and %d lines; want each once", in, f.wt.turns(t, c), f.waitLines(f.chat("c")))
	}
	f.busy(t, b)
}

// A conversation gives its place up when it blocks on a question, is stopped,
// its turn fails or its agent exits; one whose question is answered goes on
// at once, although no place is free. What waits is given in the order it
// came, and what waited in a conversation that was closed is dropped (#875).
func TestAConversationGivesItsPlaceUp(t *testing.T) {
	f := newSvTalks(t, "act", atOnce(1))
	f.wt.r.deadline = time.Now().Add(150 * time.Second)
	ctx := context.Background()
	a := f.call(t, "a", "ask which colour?")
	eventually(t, "the question", 15*time.Second, func() bool {
		d, err := f.owner.Thread(ctx, a, 0, 0)
		return err == nil && d.State == control.Waiting
	})
	b := f.long(t, "b") // blocked on a question
	f.says("a", "blue")
	f.wt.ended(t, a, 2) // blocked, then answered
	f.wt.r.postedWith(t, f.chat("a"), "answer: blue")
	f.busy(t, b)
	if n := f.waitLines(f.chat("a")) + f.waitLines(f.chat("b")); n != 0 {
		t.Fatalf("%d lines of a message that waits, want none", n)
	}

	f.says("a", "reply second")
	f.wt.r.posted(t, f.chat("a"), f.waitsFor(t, "b"))
	if n := f.wt.turns(t, a); n != 2 {
		t.Fatalf("%d turns, want the question's and the answer's", n)
	}
	if err := f.owner.Stop(ctx, b, 0); err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, a, 3) // stopped

	for i, end := range []string{"fail no reason", "exit"} {
		key := fmt.Sprintf("ends%d", i)
		c := f.call(t, key, "tool t 3s\n"+end)
		f.busy(t, c)
		f.says("a", "reply "+end)
		f.wt.r.posted(t, f.chat("a"), f.waitsFor(t, key))
		f.wt.ended(t, a, 4+i)
	}

	d := f.long(t, "d")
	e, g, h := f.call(t, "e", "reply e"), f.call(t, "g", "reply g"), f.call(t, "h", "reply h")
	for _, k := range []string{"e", "g", "h"} {
		f.wt.r.posted(t, f.chat(k), f.waitsFor(t, "d"))
	}
	if err := f.owner.CloseThread(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Stop(ctx, d, 0); err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, g, 1)
	f.wt.ended(t, h, 1)
	var first, then uint64
	for id, pos := range map[control.ThreadID]*uint64{g: &first, h: &then} {
		if err := f.wt.db.QueryRow(`SELECT min(pos) FROM events WHERE thread_id = ? AND kind = 'turn_started'`, int64(id)).Scan(pos); err != nil {
			t.Fatal(err)
		}
	}
	if then < first {
		t.Fatal("the later call was given first")
	}
	if in := f.wt.inputs(t, e); len(in) != 0 || !strings.Contains(f.log(), "input that waited for a place is dropped") {
		t.Fatalf("the closed conversation was given %q", in)
	}
}
