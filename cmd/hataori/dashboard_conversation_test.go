package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// allEvents is every event of a thread, read through ThreadEvents back to
// the thread's start, oldest first.
func allEvents(t browserTest, c *wire.Client, id control.ThreadID) []control.Event {
	t.Helper()
	var all []control.Event
	for before := uint64(0); ; {
		evs, err := c.ThreadEvents(context.Background(), id, before, control.MaxEvents)
		if err != nil {
			t.Fatal(err)
		}
		all = append(slices.Clone(evs), all...)
		if len(evs) < control.MaxEvents {
			return all
		}
		before = evs[0].Pos
	}
}

// conversationOf is the conversation the page should show of evs, as
// "kind:pos": a tool call is one entry at its start.
func conversationOf(evs []control.Event) []string {
	var out []string
	for _, e := range evs {
		switch e.Kind {
		case "turn_started", "input", "request", "answer", "text", "turn_ended":
			out = append(out, fmt.Sprintf("%s:%d", e.Kind, e.Pos))
		case "tool_started":
			out = append(out, fmt.Sprintf("tool:%d", e.Pos))
		}
	}
	return out
}

// shownConversation is the conversation the page shows at selector, as
// "kind:pos".
func (b *browser) shownConversation(selector string) []string {
	b.t.Helper()
	var out []string
	b.eval(fmt.Sprintf(`[...document.querySelectorAll('%s .conv-list > li')].map(l => l.dataset.kind + ':' + l.dataset.pos)`, selector), &out)
	return out
}

// untilConversation waits for the page's conversation at selector to be
// the server's for thread id, entry for entry, and returns it.
func (b *browser) untilConversation(what, selector string, c *wire.Client, id control.ThreadID) []string {
	b.t.Helper()
	var got, want []string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got, want = b.shownConversation(selector), conversationOf(allEvents(b.t, c, id))
		if slices.Equal(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: the page shows\n%q\nthe server has\n%q", what, got, want)
		}
	}
}

// A thread's tab shows its terminal or its conversation, driven by the
// real server with the fake agent in a terminal: a thread with a process
// opens on its terminal, an asleep, closed or failed one on its
// conversation, and the choice stays while the tab is open; the
// conversation lists inputs with who sent them, the question and its
// answer, the agent's text as text, one folded line per tool call, and the
// turns with their times; it follows new events, and after the page's
// stream is cut and comes back it has every event once.
func TestDashboardConversation(t *testing.T) {
	root := terminalRoot(t)
	once := filepath.Join(t.TempDir(), "fail-once")
	setConfig(t, root, func(cfg map[string]any) {
		ad := cfg["adapters"].(map[string]any)[fake.Name].(map[string]any)
		ad["command"] = []string{"sh", "-c", `once=$1; shift; if [ -e "$once" ]; then rm -f "$once"; exit 3; fi; exec "$@"`, "sh", once, os.Args[0], "fake"}
	})
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	repo := bareRepo(t)
	start := func(key, text string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: "k-" + key, Scope: "s", Thread: key, Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	state := func(id control.ThreadID) control.ThreadDetail {
		t.Helper()
		d, err := c.Thread(ctx, id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	settle := func(what string, id control.ThreadID, ok func(control.ThreadDetail) bool) {
		t.Helper()
		eventually(t, what, 20*time.Second, func() bool { return ok(state(id)) })
	}
	idleAfter := func(turns int) func(control.ThreadDetail) bool {
		return func(d control.ThreadDetail) bool { return d.State == control.Idle && d.Turns == turns }
	}

	os.WriteFile(once, nil, 0o600)
	failed := start("t0", "reply never")
	settle("the failed thread", failed, func(d control.ThreadDetail) bool { return d.State == control.Failed })
	const markdown = "# Title **bold** <b>x</b> [link](http://example.com) `code`"
	input := "reply " + markdown + "\ntool Bash 0s\nreply first-done"
	main := start("t1", input)
	asleep := start("t2", "reply asleep-soon\nexit")
	closing := start("t3", "reply to-close")
	settle("the main thread", main, idleAfter(1))
	settle("the asleep thread", asleep, func(d control.ThreadDetail) bool { return d.State == control.Asleep })
	settle("the thread to close", closing, idleAfter(1))
	if err := c.CloseThread(ctx, closing); err != nil {
		t.Fatal(err)
	}
	settle("the closed thread", closing, func(d control.ThreadDetail) bool { return d.State == control.Closed })
	if err := c.Send(ctx, main, "k-ask", control.Input{Text: "ask Which colour? -- red | blue"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	settle("the question", main, func(d control.ThreadDetail) bool { return d.State == control.Waiting && len(d.Requests) == 1 })
	if err := c.Answer(ctx, control.RequestID(state(main).Requests[0].ID), control.Answer{Text: "red", Choices: []string{"red"}}); err != nil {
		t.Fatal(err)
	}
	settle("the answer's turn", main, idleAfter(3))

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.showAll() // asleep and closed threads too
	pane := `.pane[data-pane="0"]`
	open := func(id control.ThreadID) {
		t.Helper()
		b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, id))
		b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, id))
		b.until("its tab", fmt.Sprintf(`!!document.querySelector('%s[data-tab="thread:%d"] .thread-pane')`, pane, id))
	}
	viewOf := func(id control.ThreadID) string {
		var v string
		b.eval(fmt.Sprintf(`document.querySelector('%s[data-tab="thread:%d"] .thread-pane')?.dataset.view ?? ''`, pane, id), &v)
		return v
	}
	untilView := func(what string, id control.ThreadID, view string) {
		t.Helper()
		b.until(what, fmt.Sprintf(`(() => {
			const p = document.querySelector('%s[data-tab="thread:%d"] .thread-pane');
			if (!p || p.dataset.view !== %q) return false;
			const pressed = p.querySelector('.view-switch [aria-pressed=true]');
			return pressed && pressed.dataset.view === %q &&
				(%q === 'terminal' ? !!p.querySelector('.xterm') && !p.querySelector('.conversation') : !!p.querySelector('.conversation') && !p.querySelector('.xterm'));
		})()`, pane, id, view, view, view))
	}

	// 1. A thread with a running process opens on its terminal; the switch
	// shows its conversation.
	open(main)
	untilView("the main thread on its terminal", main, "terminal")
	b.click(pane + ` .view-switch [data-view=conversation]`)
	untilView("the main thread's conversation", main, "conversation")
	conv := fmt.Sprintf(`%s [data-conversation="%d"]`, pane, main)
	b.untilConversation("the main thread's conversation", conv, c, main)

	// 2. What each entry shows.
	var entries []struct {
		Kind, Head, Text, Rest string
		Open                   bool
	}
	b.eval(fmt.Sprintf(`[...document.querySelectorAll('%s .conv-list > li')].map(l => ({
		kind: l.dataset.kind,
		head: l.querySelector('.conv-head, summary')?.textContent ?? l.textContent,
		text: l.querySelector('.conv-text')?.textContent ?? '',
		rest: l.querySelector('.conv-choices, .conv-tool-detail')?.textContent ?? '',
		open: !!l.querySelector('details')?.open,
	}))`, conv), &entries)
	find := func(kind, text string) int {
		return slices.IndexFunc(entries, func(e struct {
			Kind, Head, Text, Rest string
			Open                   bool
		}) bool {
			return e.Kind == kind && (text == "" || e.Text == text)
		})
	}
	if i := find("input", input); i < 0 || !strings.HasPrefix(entries[i].Head, "owner") {
		t.Fatalf("no first input from owner: %+v", entries)
	}
	if find("text", markdown) < 0 || find("text", "first-done") < 0 || find("text", "answer: red") < 0 {
		t.Fatalf("the agent's text is not shown as it is: %+v", entries)
	}
	if i := find("request", "Which colour?"); i < 0 || entries[i].Rest != "Choices: red · blue" {
		t.Fatalf("no question with its choices: %+v", entries)
	}
	if i := find("answer", "red"); i < 0 || !strings.HasPrefix(entries[i].Head, "Answer from owner") {
		t.Fatalf("no answer from owner: %+v", entries)
	}
	if i := find("tool", ""); i < 0 || entries[i].Head != "Tool Bash" || entries[i].Open {
		t.Fatalf("no folded tool line: %+v", entries)
	}
	if i := find("turn_started", ""); i < 0 || !strings.HasPrefix(entries[i].Head, "Turn 1, started by owner · ") {
		t.Fatalf("no turn start with who and when: %+v", entries)
	}
	if i := find("turn_ended", ""); i < 0 || !strings.HasPrefix(entries[i].Head, "Turn 1 ended: completion · ") {
		t.Fatalf("no turn end with its outcome and when: %+v", entries)
	}
	var markup int
	b.eval(fmt.Sprintf(`document.querySelectorAll('%s .conv-list :is(b, strong, a, h1, ul, li li)').length`, conv), &markup)
	if markup != 0 {
		t.Fatal("text in the conversation was read as markup")
	}
	b.click(conv + ` .conv-tool summary`)
	var detail string
	b.eval(fmt.Sprintf(`(() => { const d = document.querySelector('%s .conv-tool details'); return d.open ? d.querySelector('.conv-tool-detail').textContent : ''; })()`, conv), &detail)
	if !strings.HasPrefix(detail, "Started ") || !strings.Contains(detail, ", ended ") {
		t.Fatalf("the tool line, expanded: %q", detail)
	}

	// 3. Asleep, closed and failed threads open on their conversation; the
	// main thread keeps the view chosen for its tab, until the tab closes.
	for _, id := range []control.ThreadID{asleep, closing, failed} {
		open(id)
		untilView(fmt.Sprintf("thread %d on its conversation", id), id, "conversation")
	}
	b.click(fmt.Sprintf(`.tabbar .tab[data-tab="thread:%d"] .tab-main`, main))
	untilView("the main thread's tab, as chosen", main, "conversation")
	b.click(fmt.Sprintf(`.tabbar .tab[data-tab="thread:%d"] .tab-close`, main))
	b.until("the main thread's tab closed", fmt.Sprintf(`!document.querySelector('.tabbar .tab[data-tab="thread:%d"]')`, main))
	open(main)
	untilView("the main thread opened again, on its terminal", main, "terminal")
	if v := viewOf(main); v != "terminal" {
		t.Fatalf("view %q", v)
	}

	// 4. Live: a new turn's entries come without a reload.
	b.click(pane + ` .view-switch [data-view=conversation]`)
	untilView("the main thread's conversation again", main, "conversation")
	b.untilConversation("the conversation, opened again", conv, c, main)
	b.eval(`window.__samePage = true`, nil)
	if err := c.Send(ctx, main, "k-live", control.Input{Text: "reply live-one"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	settle("the live turn", main, idleAfter(4))
	b.untilConversation("the live turn's entries", conv, c, main)

	// 5. The page's stream is cut while a turn runs and ends, and comes
	// back: every event once, in order.
	// Offline, no new connection opens; stopping the page's loads ends the
	// stream that is open.
	b.call("Network.emulateNetworkConditions", map[string]any{"offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	b.eval(`window.stop(), true`, nil)
	b.until("the live mark off", `document.querySelector('.live').dataset.live === 'off'`)
	if err := c.Send(ctx, main, "k-offline", control.Input{Text: "tool Read 0s\nreply while-offline"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	settle("the turn while offline", main, idleAfter(5))
	b.call("Network.emulateNetworkConditions", map[string]any{"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
	b.until("the live mark on", `document.querySelector('.live').dataset.live === 'on'`)
	got := b.untilConversation("the turn after the reconnect", conv, c, main)
	if !slices.Contains(got, fmt.Sprintf("input:%d", lastInput(allEvents(t, c, main)))) {
		t.Fatalf("the input sent while offline is not shown: %q", got)
	}
	var same bool
	b.eval(`window.__samePage === true`, &same)
	if !same {
		t.Fatal("the page was reloaded")
	}
	var csp []string
	b.eval(`window.__csp`, &csp)
	if len(csp) > 0 {
		t.Fatalf("CSP violations: %v", csp)
	}
}

// lastInput is the position of the last input among evs.
func lastInput(evs []control.Event) uint64 {
	var pos uint64
	for _, e := range evs {
		if e.Kind == "input" {
			pos = e.Pos
		}
	}
	return pos
}

// A thread with more than 1000 events, on pipes: its tab has only the
// conversation, which starts at the end and reads older entries as it is
// scrolled to the top, until the thread's first input, each entry once.
func TestDashboardConversationLongThread(t *testing.T) {
	root := newRoot(t) // the fake agent on pipes: no terminal
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	// One subagent with 520 tools: 1040 tool events, and the turn's own.
	tools := make([]string, 520)
	for i := range tools {
		tools[i] = fmt.Sprint("t", i)
	}
	first := "subagent s1 helper " + strings.Join(tools, " ") + "\nreply long-done"
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: first}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the long turn", 60*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
	evs := allEvents(t, c, id)
	if len(evs) <= 1000 {
		t.Fatalf("only %d events", len(evs))
	}
	firstInput := uint64(0)
	for _, e := range evs {
		if e.Kind == "input" {
			firstInput = e.Pos
			break
		}
	}

	b := newBrowser(t)
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, id))
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, id))
	conv := fmt.Sprintf(`.pane[data-pane="0"] [data-conversation="%d"]`, id)
	b.until("the conversation", fmt.Sprintf(`document.querySelectorAll('%s .conv-list > li').length > 0`, conv))
	var switches int
	b.eval(`document.querySelectorAll('.pane[data-pane="0"] .view-switch').length`, &switches)
	if switches != 0 {
		t.Fatal("a session without a terminal offers a terminal view")
	}
	firstEntry := fmt.Sprintf(`%s .conv-list > li[data-kind=input][data-pos="%d"]`, conv, firstInput)
	var shownFirst bool
	b.eval(fmt.Sprintf(`!!document.querySelector('%s') || document.querySelector('%s').dataset.start === 'yes'`, firstEntry, conv), &shownFirst)
	if shownFirst {
		t.Fatal("the conversation starts at the thread's first input, not at its end")
	}
	// Scrolled to the top, again and again, as a person would.
	b.until("the first input after scrolling up", fmt.Sprintf(`(() => {
		const c = document.querySelector('%s');
		c.scrollTop = 0;
		return !!document.querySelector('%s') && c.dataset.start === 'yes';
	})()`, conv, firstEntry))
	got := b.untilConversation("the whole thread", conv, c, id)
	if len(got) < 500 {
		t.Fatalf("only %d entries", len(got))
	}
	var startText string
	b.eval(fmt.Sprintf(`document.querySelector('%s .conv-start')?.textContent ?? ''`, conv), &startText)
	if startText != "The start of the thread." {
		t.Fatalf("the start: %q", startText)
	}
}

// A caller that may not read text sees "text withheld" in place of every
// input, question, answer and text of the agent, and none of the text.
func TestDashboardConversationWithheld(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	dashboardHook = func(c *control.Caller) { c.Scopes, c.NoText = []string{"s"}, true }
	t.Cleanup(func() { dashboardHook = nil })
	runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply secret-reply\nask secret-question -- secret-choice"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the question", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Waiting && len(d.Requests) == 1
	})
	d, _ := c.Thread(ctx, id, 0, 0)
	if err := c.Answer(ctx, control.RequestID(d.Requests[0].ID), control.Answer{Text: "secret-answer"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer's turn", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Idle && d.Turns == 2
	})

	b := newBrowser(t)
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, id))
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, id))
	conv := fmt.Sprintf(`.pane[data-pane="0"] [data-conversation="%d"]`, id)
	b.untilConversation("the conversation", conv, c, id)
	var withheld map[string]int
	b.eval(fmt.Sprintf(`(() => {
		const n = {};
		for (const l of document.querySelectorAll('%s .conv-list > li')) {
			const w = l.querySelector('.withheld');
			if (w && w.textContent === 'text withheld') n[l.dataset.kind] = (n[l.dataset.kind] ?? 0) + 1;
		}
		return n;
	})()`, conv), &withheld)
	for _, kind := range []string{"input", "request", "answer", "text"} {
		if withheld[kind] == 0 {
			t.Fatalf("no %s says its text is withheld: %v", kind, withheld)
		}
	}
	var body string
	b.eval(`document.body.textContent`, &body)
	if strings.Contains(body, "secret") {
		t.Fatal("the page shows text the caller may not read")
	}
}
