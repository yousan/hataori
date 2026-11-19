package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// message_sending on the chat front end's outbox, and post_sent (#352),
// with a fake plugin connected to the daemon.

// outboxRoot is a chat root whose config lists one plugin, "p", in c1,
// handling message_sending with timeout (empty: the default) and
// onFailure (empty: the point's), with the conversation grant when
// conversation is set, and granted the post_sent events.
func outboxRoot(t *testing.T, conversation bool, timeout, onFailure string) (*chatRoot, *func(string) (string, error)) {
	t.Helper()
	dir := t.TempDir()
	h := map[string]any{"point": "message_sending"}
	if timeout != "" {
		h["timeout"] = timeout
	}
	manifest, _ := json.Marshal(map[string]any{"id": "p", "version": "0.1.0", "protocol": "3.19", "hooks": []any{h},
		"operations": []string{"Events"}, "events": []string{"post_sent"}, "conversation": conversation})
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"id": "p", "path": filepath.Join(dir, "p"), "sha256": strings.Repeat("ab", 32),
		"scopes": []string{"c1"}, "conversation": conversation}
	if onFailure != "" {
		entry["hooks"] = []map[string]any{{"point": "message_sending", "on_failure": onFailure}}
	}
	r := newChatRoot(t, func(cfg map[string]any) { cfg["plugins"] = []map[string]any{entry} })
	issue := new(func(string) (string, error))
	pluginHook = func(i func(string) (string, error), _ func(string)) { *issue = i }
	t.Cleanup(func() { pluginHook = nil })
	return r, issue
}

// postsWith counts the posts in conv whose text contains s.
func (r *chatRoot) postsWith(conv surface.Conversation, s string) int {
	n := 0
	for _, c := range r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, s) {
			n++
		}
	}
	return n
}

// cancelledOf is what `hataori thread` shows of thread th's cancelled
// posts.
func (r *chatRoot) cancelledOf(t *testing.T, th surface.Conversation) []control.CancelledPost {
	t.Helper()
	_, out, _ := cli(t, "thread", "--root", r.root, numberOf(t, r.root, th))
	var l struct{ Cancelled []control.CancelledPost }
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("hataori thread: %q: %v", out, err)
	}
	return l.Cancelled
}

// A handler rewrites one post and cancels another: the chat service gets
// the rewritten text and not the cancelled post, the posts behind it go
// on, and `hataori thread` shows the cancelled one with the plugin and the
// reason.
func TestMessageSendingRewritesAndCancels(t *testing.T) {
	r, issue := outboxRoot(t, true, "", "")
	startDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, func(q control.HookRequest) control.HookAnswer {
		switch {
		case q.Text == "REWRITE-ME":
			return control.HookAnswer{Action: control.HookContinue, Text: ptr("rewritten by the plugin")}
		case q.Text == "CANCEL-ME":
			return control.HookAnswer{Action: control.HookStop, Reason: "it is secret"}
		}
		return goOn(q)
	})
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply REWRITE-ME")
	r.posted(t, th, "rewritten by the plugin")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply CANCEL-ME")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply after")
	r.posted(t, th, "after")
	if n := r.postsWith(th, "REWRITE-ME") + r.postsWith(th, "CANCEL-ME"); n != 0 {
		t.Fatalf("%d posts of the original or the cancelled text", n)
	}
	c := r.cancelledOf(t, th)
	if len(c) != 1 || c[0].Plugin != "p" || c[0].Reason != "it is secret" || c[0].Failed || c[0].Link.Thread != th.Thread {
		t.Fatalf("hataori thread shows %+v", c)
	}
	if p.count("message_sending", "CANCEL-ME") != 1 {
		t.Fatalf("the plugin's calls: %v", p.got())
	}
}

// post_sent is written once per posted row: also when the chat service
// took a post whose answer was lost and the daemon was stopped before it
// posted it again, and never for a cancelled row. The row posted again is
// not hooked again.
func TestPostSentOncePerRow(t *testing.T) {
	r, issue := outboxRoot(t, true, "", "")
	stop := runDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	answer := func(q control.HookRequest) control.HookAnswer {
		if q.Text == "CANCEL-ME" {
			return control.HookAnswer{Action: control.HookStop, Reason: "no"}
		}
		return goOn(q)
	}
	p.connect(t, r.root, *issue, answer)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply CANCEL-ME")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply between")
	r.posted(t, th, "between")

	// The service takes the next post, and its answer is lost.
	r.driver.FailAfter(fakesurface.OpPost, 1, nil)
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply ONCE")
	eventually(t, "the post of ONCE", r.within(15*time.Second), func() bool { return r.postsWith(th, "ONCE") > 0 })
	stop()
	// Nothing is posted until the plugin is connected again, so that a
	// row hooked again would reach it.
	r.driver.Fail(fakesurface.OpPost, 1000, nil)
	runDaemon(t, r.root)
	p.connect(t, r.root, *issue, answer)
	r.driver.Fail(fakesurface.OpPost, 0, nil)
	eventually(t, "ONCE posted again", r.within(20*time.Second), func() bool { return r.postsWith(th, "ONCE") >= 2 })
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply last")
	r.posted(t, th, "last")

	sent := map[string]int{}
	var texts []string
	for _, d := range r.events(t, "post_sent", th) {
		k, _ := json.Marshal([]any{d["event"], d["part"], d["link"]})
		sent[string(k)]++
		s, _ := d["text"].(string)
		texts = append(texts, s)
	}
	for k, n := range sent {
		if n != 1 {
			t.Fatalf("post_sent for row %s %d times", k, n)
		}
	}
	count := func(s string) int {
		n := 0
		for _, x := range texts {
			if x == s {
				n++
			}
		}
		return n
	}
	if count("ONCE") != 1 || count("hello") != 1 || count("between") != 1 || count("last") != 1 || count("CANCEL-ME") != 0 {
		t.Fatalf("post_sent texts %q", texts)
	}
	if p.count("message_sending", "ONCE") != 1 {
		t.Fatalf("the row posted again was hooked again: %v", p.got())
	}
}

// A handler that times out is skipped and the post goes out unchanged;
// with on_failure block, the post is cancelled with a reason saying it
// timed out.
func TestMessageSendingTimeout(t *testing.T) {
	for _, block := range []bool{false, true} {
		t.Run(map[bool]string{false: "skip", true: "block"}[block], func(t *testing.T) {
			onFailure := ""
			if block {
				onFailure = "block"
			}
			r, issue := outboxRoot(t, true, "300ms", onFailure)
			startDaemon(t, r.root)
			r.connected(t)
			p := &fakePlugin{}
			p.connect(t, r.root, *issue, func(q control.HookRequest) control.HookAnswer {
				if q.Text == "SLOW" {
					time.Sleep(time.Second)
					return control.HookAnswer{Action: control.HookContinue, Text: ptr("late")}
				}
				return goOn(q)
			})
			th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply SLOW")
			r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply after")
			r.posted(t, th, "after")
			if r.postsWith(th, "late") != 0 {
				t.Fatal("the late answer was used")
			}
			c := r.cancelledOf(t, th)
			if !block {
				// The answer, exactly "SLOW", went out unchanged.
				if r.count(th, "SLOW") != 1 || len(c) != 0 {
					t.Fatalf("skipped: %d posts of SLOW, cancelled %+v", r.count(th, "SLOW"), c)
				}
				return
			}
			// The trigger's prompt, "reply SLOW", went out; the answer did not.
			if r.count(th, "SLOW") != 0 || len(c) != 1 || !c[0].Failed || !strings.Contains(c[0].Reason, "did not answer within 300ms") {
				t.Fatalf("blocked: %d posts of SLOW, cancelled %+v", r.count(th, "SLOW"), c)
			}
		})
	}
}

// A handler without the conversation grant gets the call without the
// text, and the post goes out as it was.
func TestMessageSendingWithoutConversation(t *testing.T) {
	const marker = "MARKER-7c2e"
	r, issue := outboxRoot(t, false, "", "")
	startDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, goOn)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply "+marker)
	r.posted(t, th, marker)
	if len(p.got()) == 0 {
		t.Fatal("the plugin was not called")
	}
	for _, c := range p.got() {
		if c.text != "" {
			t.Fatalf("a call with text: %+v", c)
		}
	}
	// And the post_sent it may read comes without the text.
	token, err := (*issue)("p")
	if err != nil {
		t.Fatal(err)
	}
	c, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	evs, err := c.Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		if e.Kind == "post_sent" {
			n++
		}
		if strings.Contains(string(e.Data), marker) {
			t.Fatalf("the plugin read %s", e.Data)
		}
	}
	if n == 0 {
		t.Fatal("the plugin read no post_sent")
	}
}

func ptr(s string) *string { return &s }
