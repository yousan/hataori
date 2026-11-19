package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// linksOf returns the links of thread n, as `hataori threads` prints them.
func linksOf(t *testing.T, root, n string) []string {
	t.Helper()
	_, out, _ := cli(t, "threads", "--root", root)
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var x struct {
			Thread int64
			Links  []struct{ Client, Channel, Thread string }
		}
		if json.Unmarshal([]byte(l), &x) != nil || strconv.FormatInt(x.Thread, 10) != n {
			continue
		}
		var out []string
		for _, k := range x.Links {
			out = append(out, k.Client+":"+k.Channel+":"+k.Thread)
		}
		return out
	}
	return nil
}

func posts(d *fakesurface.Driver, conv surface.Conversation, text string) int {
	n := 0
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpPost && c.Message.Text == text {
			n++
		}
	}
	return n
}

func opened(d *fakesurface.Driver, conv surface.Conversation) []surface.Conversation {
	var out []surface.Conversation
	for _, c := range d.Calls(conv) {
		if c.Op == fakesurface.OpOpenThread {
			out = append(out, c.Opened)
		}
	}
	return out
}

// A thread of c1 is attached on a second surface, in the channel c1's
// show_in names, by a person allowed in c1: a chat thread is opened there
// and shows the thread's answers. A guest's attach and one in a channel
// not in show_in make no link and open nothing; attaching again changes
// nothing; a conversation that shows a thread is not linked to another.
// The text form works on the second surface too.
func TestAttachAThreadOnASecondSurface(t *testing.T) {
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
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch", "grace"},
				"show_in": []string{secondSurface + ":d1", fakeSurface + ":c3"}},
			{"key": "d1", "client": secondSurface, "enabled": true},
		}
		cfg["drivers"].(map[string]any)[secondSurface] = map[string]any{"id": root + "/two"}
	})
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	eventually(t, "the second driver to connect", 10*time.Second, two.Connected)
	time.Sleep(20 * time.Millisecond)
	if cs := two.Commands(); len(cs) != 4 || cs[3].Name != "attach" {
		t.Fatalf("the second surface's commands: %+v", cs)
	}
	repo := bareRepo(t)
	th := r.order(t, 0, "!go repo:"+repo+" reply hello")
	r.posted(t, th, "hello")
	n := numberOf(t, r.root, th)
	d1, d2 := surface.Conversation{Channel: "d1"}, surface.Conversation{Channel: "d2"}
	grace, guest := fakesurface.Person("g-1"), fakesurface.Person("x-9")
	number := surface.Arg{Name: "number", Value: n}

	// Refused: a guest, and a channel not in show_in.
	two.InjectCommand(d1, guest, "attach", number)
	two.InjectCommand(d2, grace, "attach", number)
	time.Sleep(300 * time.Millisecond)
	if l, o1, o2 := linksOf(t, r.root, n), opened(two, d1), opened(two, d2); len(l) != 1 || len(o1) != 0 || len(o2) != 0 {
		t.Fatalf("refused attaches: links %v, opened %v and %v", l, o1, o2)
	}
	if got := logged(); !strings.Contains(got, `op=Link scope=c1 reason="a guest" client=`+secondSurface+" id=x-9") ||
		!strings.Contains(got, `link refused" thread=`+n+" client="+secondSurface+" channel=d2") {
		t.Fatalf("the refusals were not logged:\n%s", got)
	}

	// Allowed: grace in d1. A chat thread opens there, titled like the thread.
	two.InjectCommand(d1, grace, "attach", number)
	var there surface.Conversation
	eventually(t, "the attached thread", 15*time.Second, func() bool {
		o := opened(two, d1)
		if len(o) == 1 {
			there = o[0]
		}
		return len(o) == 1
	})
	for _, c := range two.Calls(d1) {
		if c.Op == fakesurface.OpOpenThread && c.Title != "reply hello" {
			t.Fatalf("the attached chat thread is titled %q", c.Title)
		}
	}
	if l := linksOf(t, r.root, n); len(l) != 2 || l[1] != secondSurface+":d1:"+there.Thread {
		t.Fatalf("links %v", l)
	}
	// Again: nothing changes.
	two.InjectCommand(d1, grace, "attach", surface.Arg{Name: "number", Value: "#" + n})
	time.Sleep(300 * time.Millisecond)
	if l, o := linksOf(t, r.root, n), opened(two, d1); len(l) != 2 || len(o) != 1 {
		t.Fatalf("attaching again: links %v, opened %v", l, o)
	}
	if n := posts(two, there, "Thread #"+n+" is shown here too."); n != 1 {
		t.Fatalf("the attached chat thread got %d lines saying so, want 1", n)
	}

	// The thread's next answer is shown in both.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply again")
	r.posted(t, th, "again")
	eventually(t, "the answer on the second surface", 15*time.Second, func() bool {
		for _, c := range two.Calls(there) {
			if c.Op == fakesurface.OpPost && c.Message.Text == "again" {
				return true
			}
		}
		return false
	})

	// On the first surface too, in another of its channels: the front end
	// that showed the thread in one conversation shows it in both.
	c3 := surface.Conversation{Channel: "c3"}
	r.driver.InjectCommand(c3, fakesurface.Webhook(e2eHook), "attach", number)
	var here surface.Conversation
	eventually(t, "the thread attached in c3", 15*time.Second, func() bool {
		o := opened(r.driver, c3)
		if len(o) == 1 {
			here = o[0]
		}
		return len(o) == 1
	})
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply third")
	r.posted(t, th, "third")
	eventually(t, "the answer in c3", 15*time.Second, func() bool {
		for _, c := range r.driver.Calls(here) {
			if c.Op == fakesurface.OpPost && c.Message.Text == "third" {
				return true
			}
		}
		return false
	})

	// A second thread: not into the conversation that shows the first,
	// and by the text form into a chat thread of its own.
	th2 := r.order(t, 1, "!go repo:"+repo+" reply second")
	r.posted(t, th2, "second")
	n2 := numberOf(t, r.root, th2)
	two.InjectCommand(there, grace, "attach", surface.Arg{Name: "number", Value: n2})
	two.Inject(d1, grace, "!go attach "+n2)
	eventually(t, "the second thread attached by text", 15*time.Second, func() bool { return len(linksOf(t, r.root, n2)) == 2 })
	time.Sleep(200 * time.Millisecond)
	if l, l2, o := linksOf(t, r.root, n), linksOf(t, r.root, n2), opened(two, d1); len(l) != 3 || len(l2) != 2 || len(o) != 2 ||
		strings.HasSuffix(l2[1], ":"+there.Thread) {
		t.Fatalf("links %v and %v, opened %v", l, l2, o)
	}
	if !strings.Contains(logged(), `link refused" thread=`+n2+" client="+secondSurface+" channel=d1") {
		t.Fatalf("the conversation of another thread was not refused:\n%s", logged())
	}
}
