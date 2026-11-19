package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/plugin"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// The input hook points (#351) with a fake plugin connected to the
// daemon: before_agent_run and before_prompt_build, inside Start and Send.

// hookCall is one hook request a fake plugin got.
type hookCall struct{ point, text string }

// fakePlugin is a plugin of the config, connected by the test with the
// token the daemon issued, that answers hook requests with answer.
type fakePlugin struct {
	mu    sync.Mutex
	calls []hookCall
}

func (p *fakePlugin) got() []hookCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// count is how many calls at point had text containing s.
func (p *fakePlugin) count(point, s string) int {
	n := 0
	for _, c := range p.got() {
		if c.point == point && strings.Contains(c.text, s) {
			n++
		}
	}
	return n
}

// hookRoot is a chat root whose config lists one plugin, "p", in c1, with
// the conversation grant and the manifest's hooks, each a {point,
// timeout} pair; issue is set to what issues its token, at each start of
// the daemon.
func hookRoot(t *testing.T, hooks ...[2]string) (*chatRoot, *func(string) (string, error)) {
	t.Helper()
	dir := t.TempDir()
	var hs []map[string]any
	for _, h := range hooks {
		m := map[string]any{"point": h[0]}
		if h[1] != "" {
			m["timeout"] = h[1]
		}
		hs = append(hs, m)
	}
	manifest, _ := json.Marshal(map[string]any{"id": "p", "version": "0.1.0", "protocol": "3.17", "hooks": hs, "conversation": true})
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["plugins"] = []map[string]any{{"id": "p", "path": filepath.Join(dir, "p"), "sha256": strings.Repeat("ab", 32),
			"scopes": []string{"c1"}, "conversation": true}}
	})
	issue := new(func(string) (string, error))
	pluginHook = func(i func(string) (string, error), _ func(string)) { *issue = i }
	t.Cleanup(func() { pluginHook = nil })
	return r, issue
}

// connect connects p as the plugin, and returns once the daemon told it
// plugin_started, so that its hooks are live.
func (p *fakePlugin) connect(t *testing.T, root string, issue func(string) (string, error),
	answer func(r control.HookRequest) control.HookAnswer) {
	t.Helper()
	token, err := issue("p")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	c, err := wire.DialTokenHandler(root, token, func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		switch op {
		case control.OpPluginStarted:
			select {
			case started <- struct{}{}:
			default:
			}
			return nil, nil
		case control.OpHook:
			var r control.HookRequest
			if err := json.Unmarshal(args, &r); err != nil {
				return nil, err
			}
			p.mu.Lock()
			p.calls = append(p.calls, hookCall{r.Point, r.Text})
			p.mu.Unlock()
			return answer(r), nil
		}
		return nil, wire.ErrNotSupported
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the plugin was not told plugin_started")
	}
}

func goOn(control.HookRequest) control.HookAnswer {
	return control.HookAnswer{Action: control.HookContinue}
}

// texts are the agent's texts in thread th, in order.
func (r *chatRoot) texts(t *testing.T, th surface.Conversation) []string {
	t.Helper()
	var out []string
	for _, d := range r.events(t, "text", th) {
		s, _ := d["text"].(string)
		out = append(out, s)
	}
	return out
}

// replyTo waits for a post in conv that replies to the message ref and
// has text, and returns how many there are.
func (r *chatRoot) repliesTo(t *testing.T, conv surface.Conversation, ref surface.MessageRef, text string) int {
	t.Helper()
	n := 0
	eventually(t, "a reply of "+text, r.within(15*time.Second), func() bool {
		n = 0
		for _, c := range r.driver.Calls(conv) {
			if c.Op == fakesurface.OpPost && c.Message.Text == text && c.Message.ReplyTo != nil && *c.Message.ReplyTo == ref {
				n++
			}
		}
		return n > 0
	})
	time.Sleep(200 * time.Millisecond) // a second one would be written by now
	n = 0
	for _, c := range r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && c.Message.Text == text && c.Message.ReplyTo != nil && *c.Message.ReplyTo == ref {
			n++
		}
	}
	return n
}

// opened is how many chat threads were opened in c1.
func (r *chatRoot) openedThreads() int {
	n := 0
	for _, c := range r.driver.Calls(c1) {
		if c.Op == fakesurface.OpOpenThread {
			n++
		}
	}
	return n
}

// A before_agent_run handler stops a Start and a Send. The stopped Start
// opens no chat thread and no workspace, and its trigger gets one reply
// naming the plugin and the reason. The stopped Send never reaches the
// agent, writes no input event, and the thread gets one line naming the
// plugin and the reason.
func TestBeforeAgentRunStopsInput(t *testing.T) {
	r, issue := hookRoot(t, [2]string{"before_agent_run", ""})
	startDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, func(q control.HookRequest) control.HookAnswer {
		if strings.Contains(q.Text, "SECRET") {
			return control.HookAnswer{Action: control.HookStop, Reason: "it holds a secret"}
		}
		return goOn(q)
	})
	repo := bareRepo(t)

	trigger := r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply SECRET-start")
	if n := r.repliesTo(t, c1, trigger.Ref, "Nothing was started: the plugin p stopped it: it holds a secret"); n != 1 {
		t.Fatalf("%d replies to the stopped trigger, want 1", n)
	}
	if n := r.openedThreads(); n != 0 {
		t.Fatalf("a stopped Start opened %d chat threads", n)
	}
	if ws, _ := os.ReadDir(filepath.Join(r.root, workspace.Dir)); len(ws) != 0 {
		t.Fatalf("a stopped Start made workspaces: %v", ws)
	}
	if _, out, _ := cli(t, "threads", "--root", r.root); strings.TrimSpace(out) != "" {
		t.Fatalf("a stopped Start made a thread: %s", out)
	}

	th := r.order(t, 0, "!go repo:"+repo+" reply hello")
	r.posted(t, th, "hello")
	msg := r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply SECRET-send")
	if n := r.repliesTo(t, th, msg.Ref, "Not sent to the agent: the plugin p stopped it: it holds a secret"); n != 1 {
		t.Fatalf("%d notices of the stopped Send, want 1", n)
	}
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply after")
	r.posted(t, th, "after")
	if got := r.texts(t, th); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "SECRET") }) {
		t.Fatalf("the agent got the stopped input: its texts %q", got)
	}
	for _, d := range r.events(t, "input", th) {
		if s, _ := d["text"].(string); strings.Contains(s, "SECRET") {
			t.Fatalf("an input event for the stopped input: %v", d)
		}
	}
	if p.count("before_agent_run", "SECRET-send") != 1 || p.count("before_agent_run", "reply after") != 1 {
		t.Fatalf("the plugin's calls: %v", p.got())
	}
}

// A before_prompt_build handler adds one line before the input and one
// after: the agent gets both around it, for a Start and for a Send, and
// the input event has the input as given in text and the added lines in
// before and after. The plugin sees the input as given.
func TestBeforePromptBuildAddsAround(t *testing.T) {
	r, issue := hookRoot(t, [2]string{"before_prompt_build", ""})
	startDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, func(q control.HookRequest) control.HookAnswer {
		return control.HookAnswer{Action: control.HookContinue, Before: "reply BEFORE-LINE", After: "reply AFTER-LINE"}
	})
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply START")
	r.posted(t, th, "AFTER-LINE")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply SEND")
	eventually(t, "the second turn's texts", r.within(15*time.Second), func() bool { return len(r.texts(t, th)) >= 6 })
	if got, want := r.texts(t, th), []string{"BEFORE-LINE", "START", "AFTER-LINE", "BEFORE-LINE", "SEND", "AFTER-LINE"}; !slices.Equal(got, want) {
		t.Fatalf("the agent said %q, want %q", got, want)
	}
	ins := r.events(t, "input", th)
	if len(ins) != 2 {
		t.Fatalf("input events %v", ins)
	}
	for i, text := range []string{"reply START", "reply SEND"} {
		if ins[i]["text"] != text || ins[i]["before"] != "reply BEFORE-LINE" || ins[i]["after"] != "reply AFTER-LINE" {
			t.Fatalf("input event %d: %v", i, ins[i])
		}
	}
	if p.count("before_prompt_build", "reply START") != 1 || p.count("before_prompt_build", "BEFORE") != 0 {
		t.Fatalf("the plugin's calls: %v", p.got())
	}
	// The thread shows the prompt as the person wrote it.
	r.posted(t, th, "reply START")
}

// A before_agent_run handler that times out blocks the input; a
// before_prompt_build handler that times out is skipped, and the input
// goes on as given.
func TestInputHookTimeouts(t *testing.T) {
	r, issue := hookRoot(t, [2]string{"before_agent_run", "300ms"}, [2]string{"before_prompt_build", "300ms"})
	startDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, func(q control.HookRequest) control.HookAnswer {
		slow := q.Point == "before_agent_run" && strings.Contains(q.Text, "SLOW-GATE") ||
			q.Point == "before_prompt_build" && strings.Contains(q.Text, "SLOW-BUILD")
		if slow {
			time.Sleep(time.Second)
			return control.HookAnswer{Action: control.HookContinue, Before: "reply LATE"}
		}
		return goOn(q)
	})
	repo := bareRepo(t)
	trigger := r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply SLOW-GATE")
	want := "Nothing was started: the plugin p failed (it did not answer within 300ms), and its policy blocks the input."
	if n := r.repliesTo(t, c1, trigger.Ref, want); n != 1 {
		t.Fatalf("%d replies to the blocked trigger", n)
	}
	if n := r.openedThreads(); n != 0 {
		t.Fatalf("a blocked Start opened %d chat threads", n)
	}
	th := r.order(t, 0, "!go repo:"+repo+" reply SLOW-BUILD")
	r.posted(t, th, "SLOW-BUILD")
	if got := r.texts(t, th); !slices.Equal(got, []string{"SLOW-BUILD"}) {
		t.Fatalf("the agent said %q", got)
	}
	ins := r.events(t, "input", th)
	if len(ins) != 1 || ins[0]["before"] != nil || ins[0]["after"] != nil {
		t.Fatalf("input events %v", ins)
	}
}

// A daemon restart does not call a hook twice for one accepted input: a
// Start and a Send given again with their keys after the restart run no
// hook, and the agent gets each once.
func TestRestartRunsNoHookTwice(t *testing.T) {
	r, issue := hookRoot(t, [2]string{"before_agent_run", ""}, [2]string{"before_prompt_build", ""})
	stop := runDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, goOn)
	ctx := context.Background()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	start := control.StartRequest{Key: "k-restart", Scope: "c1", Thread: "t-restart", Repo: bareRepo(t), Input: control.Input{Text: "reply ONCE-START"}}
	id, err := owner.Start(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	ended := func(n int) {
		eventually(t, "the turn's end", 20*time.Second, func() bool { return len(threadEvents(t, owner, id, "turn_ended")) >= n })
	}
	ended(1)
	if err := owner.Send(ctx, id, "s-restart", control.Input{Text: "reply ONCE-SEND"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	ended(2)
	owner.Close()
	stop()

	runDaemon(t, r.root)
	p.connect(t, r.root, *issue, goOn)
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if again, err := owner.Start(ctx, start); err != nil || again != id {
		t.Fatalf("the Start again: %d, %v", again, err)
	}
	if err := owner.Send(ctx, id, "s-restart", control.Input{Text: "reply ONCE-SEND"}, control.Refuse); err != nil {
		t.Fatalf("the Send again: %v", err)
	}
	// And a new input runs them, so the plugin is connected and called.
	if err := owner.Send(ctx, id, "s-new", control.Input{Text: "reply NEW"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	ended(3)
	for _, point := range []string{"before_agent_run", "before_prompt_build"} {
		for _, s := range []string{"ONCE-START", "ONCE-SEND", "NEW"} {
			if n := p.count(point, s); n != 1 {
				t.Fatalf("%s was called %d times for %s: %v", point, n, s, p.got())
			}
		}
	}
	var said []string
	for _, d := range threadEvents(t, owner, id, "text") {
		s, _ := d["text"].(string)
		said = append(said, s)
	}
	if !slices.Equal(said, []string{"ONCE-START", "ONCE-SEND", "NEW"}) {
		t.Fatalf("the agent said %q", said)
	}
}

// An answer to an open question gets only the gate: before_agent_run is
// called for it, before_prompt_build is not, and the agent gets it as
// given.
func TestAnAnswerGetsOnlyTheGate(t *testing.T) {
	r, issue := hookRoot(t, [2]string{"before_agent_run", ""}, [2]string{"before_prompt_build", ""})
	startDaemon(t, r.root)
	r.connected(t)
	p := &fakePlugin{}
	p.connect(t, r.root, *issue, func(q control.HookRequest) control.HookAnswer {
		if q.Point == "before_prompt_build" {
			return control.HookAnswer{Action: control.HookContinue, Before: "reply ADDED"}
		}
		return goOn(q)
	})
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" ask Which colour?")
	r.posted(t, th, "Which colour?")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "green")
	r.posted(t, th, "answer: green")
	if p.count("before_agent_run", "green") != 1 || p.count("before_prompt_build", "green") != 0 {
		t.Fatalf("the plugin's calls: %v", p.got())
	}
}
