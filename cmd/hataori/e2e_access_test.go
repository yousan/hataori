package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The end-to-end suite for who may do what (design, section 8).

// opened reports the threads opened in conv.
func (r *chatRoot) opened(conv surface.Conversation) int {
	n := 0
	for _, c := range r.driver.Calls(conv) {
		if c.Op == fakesurface.OpOpenThread {
			n++
		}
	}
	return n
}

// Two instances on one host, each with a root and a bot of its own, see
// the same messages; each starts threads only in its own scopes, and
// clones only under its own root.
func TestTwoInstancesIgnoreEachOthersScopes(t *testing.T) {
	scope := func(key string) func(map[string]any) {
		return func(cfg map[string]any) {
			cfg["scopes"] = []map[string]any{{"key": key, "client": fakeSurface, "enabled": true, "posters": []string{e2eHook}}}
		}
	}
	a, b := newChatRoot(t, scope("ca")), newChatRoot(t, scope("cb"))
	startDaemon(t, a.root)
	startDaemon(t, b.root)
	a.connected(t)
	b.connected(t)
	repo := bareRepo(t)
	ca, cb := surface.Conversation{Channel: "ca"}, surface.Conversation{Channel: "cb"}
	for _, r := range []*chatRoot{a, b} { // the service delivers every message to both bots
		r.driver.Inject(ca, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply for a")
		r.driver.Inject(cb, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply for b")
	}
	eventually(t, "a thread in each", 15*time.Second, func() bool { return a.opened(ca) == 1 && b.opened(cb) == 1 })
	openedIn := func(r *chatRoot, conv surface.Conversation) surface.Conversation {
		for _, c := range r.driver.Calls(conv) {
			if c.Op == fakesurface.OpOpenThread {
				return c.Opened
			}
		}
		return surface.Conversation{}
	}
	// Each answers in its own scope: its turn ran, so its clone is made.
	ta, tb := openedIn(a, ca), openedIn(b, cb)
	a.posted(t, ta, "for a")
	b.posted(t, tb, "for b")
	time.Sleep(200 * time.Millisecond)
	if a.opened(cb) != 0 || b.opened(ca) != 0 {
		t.Fatalf("a opened %d threads in b's scope, b %d in a's", a.opened(cb), b.opened(ca))
	}
	for _, x := range []struct {
		r          *chatRoot
		th         surface.Conversation
		own, other string
	}{{a, ta, "ca", "cb"}, {b, tb, "cb", "ca"}} {
		clone := filepath.Join(x.r.root, "workspaces", fakeSurface, x.own, x.th.Thread)
		if _, err := os.Stat(filepath.Join(clone, "README")); err != nil {
			t.Fatalf("no clone at %s: %v", clone, err)
		}
		own, _ := filepath.Glob(filepath.Join(x.r.root, "workspaces", fakeSurface, x.own, "*"))
		other, _ := filepath.Glob(filepath.Join(x.r.root, "workspaces", fakeSurface, x.other, "*"))
		if len(own) != 1 || len(other) != 0 {
			t.Fatalf("clones under %s: %v in its scope, %v in the other's", x.r.root, own, other)
		}
	}
}

// A trigger from an author who is not allowed, from a bot account, and in
// a scope that is not enabled each start nothing and write one log line.
func TestRefusedTriggersStartNothingAndLogOneLine(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"] = append(cfg["scopes"].([]map[string]any), map[string]any{"key": "c2", "client": fakeSurface, "enabled": false})
	})
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	c2 := surface.Conversation{Channel: "c2"}
	r.driver.Inject(c1, fakesurface.Webhook("stranger"), "!go repo:"+repo+" reply no") // a guest
	r.driver.Inject(c1, fakesurface.Bot(e2eHook), "!go repo:"+repo+" reply no")
	r.driver.Inject(c2, fakesurface.Person(e2eOwner), "!go repo:"+repo+" reply no")
	r.order(t, 0, "!go repo:"+repo+" reply yes") // after the three: once it runs, they were read
	if n := r.opened(c1) + r.opened(c2); n != 1 {
		t.Fatalf("%d threads opened, want only the allowed one", n)
	}
	for _, reason := range []string{"a guest", "the author is a bot account", "the scope is not enabled"} {
		if n := strings.Count(logged(), "reason=\""+reason+"\""); n != 1 {
			t.Errorf("%d log lines say %q, want 1", n, reason)
		}
	}
}

// `hataori thread <key>` finds a thread hataori started, and tells that a
// thread of the chat service that hataori did not start is not one.
func TestThreadCommandTellsAHataoriThread(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hi")
	r.posted(t, th, "hi")
	ordinary, _ := r.driver.OpenThread(nil, c1, "a thread of people")
	if code, out, _ := cli(t, "thread", "--root", r.root, th.Thread); code != 0 || !strings.Contains(out, `"key":"`+th.Thread+`"`) || !strings.Contains(out, `"state":`) {
		t.Fatalf("thread %s: exit %d, %s", th.Thread, code, out)
	}
	if code, out, _ := cli(t, "thread", "--root", r.root, ordinary.Thread); code != exitNoThread || out != "" {
		t.Fatalf("thread %s (not hataori's): exit %d, %q", ordinary.Thread, code, out)
	}
}

// A guest's trigger opens no thread and starts nothing, and a guest's
// message in a running thread never reaches the agent: no input event, no
// reply, while the same message from one of the scope's people does.
func TestGuestInputDoesNotReachTheAgent(t *testing.T) {
	r := newChatRoot(t, nil)
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	r.driver.Inject(c1, fakesurface.Person("guest-1"), "!go repo:"+repo+" reply from a guest")
	th := r.order(t, 0, "!go repo:"+repo+" reply hello") // after the guest's: once it runs, that was read
	r.posted(t, th, "hello")
	if n := r.opened(c1); n != 1 {
		t.Fatalf("%d threads opened, want only the allowed one", n)
	}
	r.driver.Inject(th, fakesurface.Person("guest-1"), "reply leaked")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply allowed")
	r.posted(t, th, "allowed")
	for _, c := range r.driver.Calls(th) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "leaked") {
			t.Fatalf("the agent answered a guest: %+v", c)
		}
	}
	for _, in := range r.events(t, "input", th) {
		if strings.Contains(fmt.Sprint(in["text"]), "leaked") || strings.Contains(fmt.Sprint(in["text"]), "from a guest") {
			t.Fatalf("a guest's input reached the agent: %v", in)
		}
	}
	if n := strings.Count(logged(), `reason="a guest" client=`+fakeSurface+` id=guest-1`); n != 2 {
		t.Errorf("%d refusals of the guest logged, want 2 (the trigger and the message)\n%s", n, logged())
	}
}

// A person listed with an id on a second chat service is accepted there:
// their order on the second surface starts a thread and is answered. The
// owner's id from the first service is a guest on the second one.
func TestAllowedPersonOnASecondSurface(t *testing.T) {
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
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch"}},
			{"key": "d1", "client": secondSurface, "enabled": true, "people": []string{"grace"}},
		}
		cfg["drivers"].(map[string]any)[secondSurface] = map[string]any{"id": root + "/two"}
	})
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	eventually(t, "the second driver to connect", 10*time.Second, two.Connected)
	time.Sleep(20 * time.Millisecond)
	repo := bareRepo(t)
	d1 := surface.Conversation{Channel: "d1"}
	two.Inject(d1, fakesurface.Person(e2eOwner), "!go repo:"+repo+" reply owner id on the wrong service")
	two.Inject(d1, fakesurface.Person("g-1"), "!go repo:"+repo+" reply from grace")
	var th surface.Conversation
	eventually(t, "a thread on the second surface", 15*time.Second, func() bool {
		for _, c := range two.Calls(d1) {
			if c.Op == fakesurface.OpOpenThread {
				th = c.Opened
				return true
			}
		}
		return false
	})
	eventually(t, "grace's answer", 15*time.Second, func() bool {
		for _, c := range two.Calls(th) {
			if c.Op == fakesurface.OpPost && c.Message.Text == "from grace" {
				return true
			}
		}
		return false
	})
	time.Sleep(200 * time.Millisecond)
	n := 0
	for _, c := range two.Calls(d1) {
		if c.Op == fakesurface.OpOpenThread {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d threads opened on the second surface, want grace's only", n)
	}
	started := r.events(t, "thread_created", th)
	if len(started) != 1 || started[0]["principal"] != "grace" || started[0]["from"] != secondSurface+":g-1" {
		t.Fatalf("thread_created %v", started)
	}
	if !strings.Contains(logged(), `reason="a guest" client=`+secondSurface+` id=`+e2eOwner) {
		t.Errorf("the owner's id from the first service was not refused as a guest on the second:\n%s", logged())
	}
}
