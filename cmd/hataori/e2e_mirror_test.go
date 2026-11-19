package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// postsLike returns the posts in conv whose text is text.
func postsLike(d *fakesurface.Driver, conv surface.Conversation, text string) []fakesurface.Call {
	var out []fakesurface.Call
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpPost && c.Message.Text == text {
			out = append(out, c)
		}
	}
	return out
}

// A thread with two links on two surfaces: input in either appears once in
// the other as "<person>: <text>", and not again where it was written; the
// answer appears in both, as a reply only where the input was written. A
// guest's message, a refused one and one in a closed thread appear only
// where they were written.
func TestEveryLinkReadsTheSameConversation(t *testing.T) {
	var two *fakesurface.Driver
	r := newChatRoot(t, func(cfg map[string]any) {
		root := cfg["drivers"].(map[string]any)[fakeSurface].(map[string]any)["id"].(string)
		two = fakesurface.New()
		drivers.Store(root+"/two", two)
		t.Cleanup(func() { drivers.Delete(root + "/two") })
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
			{"name": "grace", "ids": []string{secondSurface + ":g-1"}},
			{"name": "eve", "ids": []string{secondSurface + ":e-1"}},
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch", "grace"},
				"show_in": []string{secondSurface + ":d1", fakeSurface + ":c3"}},
			{"key": "d1", "client": secondSurface, "enabled": true, "people": []string{"eve"}},
		}
		cfg["drivers"].(map[string]any)[secondSurface] = map[string]any{"id": root + "/two"}
	})
	startDaemon(t, r.root)
	r.connected(t)
	eventually(t, "the second driver to connect", 10*time.Second, two.Connected)
	time.Sleep(20 * time.Millisecond)
	one := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, one, "hello")
	n := numberOf(t, r.root, one)
	d1 := surface.Conversation{Channel: "d1"}
	two.InjectCommand(d1, fakesurface.Person("g-1"), "attach", surface.Arg{Name: "number", Value: n})
	var there surface.Conversation
	eventually(t, "the second link", 15*time.Second, func() bool {
		o := opened(two, d1)
		if len(o) == 1 {
			there = o[0]
		}
		return len(o) == 1
	})
	eventually(t, "the link stored", 10*time.Second, func() bool { return len(linksOf(t, r.root, n)) == 2 })

	// Through the second link: shown once in the first, answered in both.
	in2 := two.Inject(there, fakesurface.Person("g-1"), "reply from two")
	eventually(t, "the answer in both", 15*time.Second, func() bool {
		return len(postsLike(two, there, "from two")) == 1 && len(postsLike(r.driver, one, "from two")) == 1
	})
	if got := postsLike(r.driver, one, "grace: reply from two"); len(got) != 1 {
		t.Fatalf("the first link shows grace's input %d times, want once", len(got))
	}
	if got := postsLike(two, there, "grace: reply from two"); len(got) != 0 {
		t.Fatalf("the second link got its own input back %d times", len(got))
	}
	if a := postsLike(two, there, "from two")[0]; a.Message.ReplyTo == nil || a.Message.ReplyTo.ID != in2.Ref.ID {
		t.Fatalf("the answer in the second link replies to %+v, want grace's message", a.Message.ReplyTo)
	}
	if a := postsLike(r.driver, one, "from two")[0]; a.Message.ReplyTo != nil {
		t.Fatalf("the answer in the first link replies to %+v, a message of the second", a.Message.ReplyTo)
	}

	// Through the first link: shown once in the second.
	r.driver.Inject(one, fakesurface.Webhook(e2eHook), "reply from one")
	eventually(t, "the answer in both", 15*time.Second, func() bool {
		return len(postsLike(two, there, "from one")) == 1 && len(postsLike(r.driver, one, "from one")) == 1
	})
	if got, back := postsLike(two, there, "dispatch: reply from one"), postsLike(r.driver, one, "dispatch: reply from one"); len(got) != 1 || len(back) != 0 {
		t.Fatalf("dispatch's input shown %d times in the second link and %d in the first", len(got), len(back))
	}

	// A second link on the first surface: its front end shows the thread in
	// two conversations, and the answer replies in the one the input came
	// through.
	c3 := surface.Conversation{Channel: "c3"}
	r.driver.InjectCommand(c3, fakesurface.Webhook(e2eHook), "attach", surface.Arg{Name: "number", Value: n})
	var here surface.Conversation
	eventually(t, "the link in c3", 15*time.Second, func() bool {
		o := opened(r.driver, c3)
		if len(o) == 1 {
			here = o[0]
		}
		return len(o) == 1 && len(linksOf(t, r.root, n)) == 3
	})
	in3 := r.driver.Inject(here, fakesurface.Webhook(e2eHook), "reply from three")
	eventually(t, "the answer in all three", 15*time.Second, func() bool {
		return len(postsLike(r.driver, here, "from three")) == 1 && len(postsLike(r.driver, one, "from three")) == 1 &&
			len(postsLike(two, there, "from three")) == 1
	})
	if a := postsLike(r.driver, here, "from three")[0]; a.Message.ReplyTo == nil || a.Message.ReplyTo.ID != in3.Ref.ID {
		t.Fatalf("the answer in c3 replies to %+v, want the input written there", a.Message.ReplyTo)
	}
	if a := postsLike(r.driver, one, "from three")[0]; a.Message.ReplyTo != nil {
		t.Fatalf("the answer in the first link replies to %+v", a.Message.ReplyTo)
	}
	if a, b := postsLike(r.driver, one, "dispatch: reply from three"), postsLike(two, there, "dispatch: reply from three"); len(a) != 1 || len(b) != 1 {
		t.Fatalf("the input of c3 is shown %d and %d times in the other links, want once each", len(a), len(b))
	}

	// Not given to the agent: a guest's message, one of a person not of
	// the thread's scope, and one in the closed thread.
	two.Inject(there, fakesurface.Person("x-9"), "reply guest")
	two.Inject(there, fakesurface.Person("e-1"), "reply eve")
	time.Sleep(500 * time.Millisecond)
	if code, _, errOut := cli(t, "close", "--root", r.root, n); code != 0 {
		t.Fatalf("hataori close: %d %s", code, errOut)
	}
	two.Inject(there, fakesurface.Person("g-1"), "reply too late")
	time.Sleep(500 * time.Millisecond)
	for _, c := range r.driver.Calls(one) {
		if c.Op == fakesurface.OpPost && (strings.Contains(c.Message.Text, "guest") || strings.Contains(c.Message.Text, "eve") ||
			strings.Contains(c.Message.Text, "too late")) {
			t.Fatalf("the first link shows a message that did not reach the agent: %q", c.Message.Text)
		}
	}
}
