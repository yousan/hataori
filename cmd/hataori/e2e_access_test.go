package main

import (
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
	time.Sleep(200 * time.Millisecond)
	if a.opened(cb) != 0 || b.opened(ca) != 0 {
		t.Fatalf("a opened %d threads in b's scope, b %d in a's", a.opened(cb), b.opened(ca))
	}
	for _, r := range []struct {
		root       *chatRoot
		own, other string
	}{{a, "ca", "cb"}, {b, "cb", "ca"}} {
		own, _ := filepath.Glob(filepath.Join(r.root.root, "workspaces", fakeSurface, r.own, "*"))
		other, _ := filepath.Glob(filepath.Join(r.root.root, "workspaces", fakeSurface, r.other, "*"))
		if len(own) != 1 || len(other) != 0 {
			t.Fatalf("clones under %s: %v in its scope, %v in the other's", r.root.root, own, other)
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
	r.driver.Inject(c1, fakesurface.Webhook("stranger"), "!go repo:"+repo+" reply no")
	r.driver.Inject(c1, fakesurface.Bot(e2eHook), "!go repo:"+repo+" reply no")
	r.driver.Inject(c2, fakesurface.Person(e2eOwner), "!go repo:"+repo+" reply no")
	r.order(t, 0, "!go repo:"+repo+" reply yes") // after the three: once it runs, they were read
	if n := r.opened(c1) + r.opened(c2); n != 1 {
		t.Fatalf("%d threads opened, want only the allowed one", n)
	}
	for _, reason := range []string{"the author is not allowed", "the author is a bot account", "the scope is not enabled"} {
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
