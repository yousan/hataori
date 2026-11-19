package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/wire"
)

// esc is the escape character; the fake agent's "screen" writes it to its
// terminal unchanged.
const esc = "\x1b"

// terminalQueries are sequences a terminal answers by sending bytes back:
// primary and secondary device attributes, the cursor position, the
// status, the window size, and the colours.
var terminalQueries = esc + "[c" + esc + "[>c" + esc + "[6n" + esc + "[5n" + esc + "[18t" + esc + "]10;?\x07" + esc + "]11;?\x07"

// pageThread starts the dashboard's daemon and one thread with input, and
// opens its tab in a browser that records the page's requests and any
// Content-Security-Policy violation.
type paneRig struct {
	t    *testing.T
	root string
	c    *wire.Client
	b    *browser
	id   control.ThreadID
	n    int64
	log  func() string
	base string // the sign-in link the browser opened
}

func newPaneRig(t *testing.T, root string, input string, settled func(control.ThreadInfo) bool) *paneRig {
	t.Helper()
	_, logged := runDaemonLog(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: input}})
	if err != nil {
		t.Fatal(err)
	}
	r := &paneRig{t: t, root: root, c: c, id: id, log: logged}
	eventually(t, "the thread to settle", 20*time.Second, func() bool { x := r.info(); r.n = int64(x.ID); return settled(x) })

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI + ' ' + (e.sample || '')));`})
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	r.base = link
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, r.n))
	b.eval(fmt.Sprintf(`document.querySelector('[data-section=threads] .row[data-thread="%d"]').click(), true`, r.n), nil)
	r.b = b
	return r
}

func (r *paneRig) info() control.ThreadInfo {
	r.t.Helper()
	d, err := r.c.Thread(context.Background(), r.id, 0, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return d.ThreadInfo
}

// screen is the text of the pane's terminal, row by row.
func (r *paneRig) screen() string {
	r.t.Helper()
	var s string
	r.b.eval(fmt.Sprintf(`[...document.querySelectorAll('[data-terminal="%d"] .xterm-rows > div')].map(d => d.textContent.trimEnd()).join('\n')`, r.n), &s)
	return s
}

func (r *paneRig) untilScreen(what string, ok func(string) bool) string {
	r.t.Helper()
	var s string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if s = r.screen(); ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the terminal never showed %s:\n%s", what, s)
		}
	}
}

// noViolations fails when the page broke its Content-Security-Policy.
func (r *paneRig) noViolations() {
	r.t.Helper()
	var v []string
	r.b.eval(`window.__csp`, &v)
	if len(v) > 0 {
		r.t.Fatalf("Content-Security-Policy violations: %q", v)
	}
}

// The terminal pane sends nothing back. Real keys, a paste, typed text and
// the terminal's answers to queries are all dropped: no request the page
// makes carries a byte of them, every request is a GET or HEAD of the
// page's own routes, no WebSocket is opened, and the session gets no input.
func TestDashboardTerminalPaneIsReadOnly(t *testing.T) {
	root := terminalRoot(t)
	r := newPaneRig(t, root, "screen ready-marker\nscreen "+terminalQueries+"query-marker\nreply one",
		func(x control.ThreadInfo) bool { return x.State == control.Idle && x.Turns == 1 })
	b := r.b
	r.untilScreen("the output and the queries", func(s string) bool { return strings.Contains(s, "ready-marker") && strings.Contains(s, "query-marker") })

	// Keys, typed into the terminal as a person would.
	b.eval(fmt.Sprintf(`document.querySelector('[data-terminal="%d"] .xterm').click(), document.querySelector('[data-terminal="%d"] textarea').focus(), true`, r.n, r.n), nil)
	var focused bool
	b.eval(`document.activeElement?.classList.contains('xterm-helper-textarea') ?? false`, &focused)
	if !focused {
		t.Fatal("the terminal's input element does not have the focus: the keys would test nothing")
	}
	press := func(key, code string, vk int, text string, mods int) {
		t.Helper()
		down := map[string]any{"type": "keyDown", "key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": mods}
		if text != "" {
			down["text"], down["unmodifiedText"] = text, text
		}
		b.call("Input.dispatchKeyEvent", down)
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": mods})
	}
	for _, ch := range "typed-keys" {
		s := string(ch)
		press(s, "Key"+strings.ToUpper(s), int(strings.ToUpper(s)[0]), s, 0)
	}
	press("Enter", "Enter", 13, "\r", 0)
	press("c", "KeyC", 67, "", 2) // Ctrl+C
	press("Escape", "Escape", 27, "", 0)
	press("ArrowUp", "ArrowUp", 38, "", 0)
	press("Tab", "Tab", 9, "", 0)
	b.eval(fmt.Sprintf(`document.querySelector('[data-terminal="%d"] textarea').focus(), true`, r.n), nil)
	b.call("Input.insertText", map[string]any{"text": "inserted-text"})
	b.eval(fmt.Sprintf(`(() => {
		const ta = document.querySelector('[data-terminal="%d"] textarea');
		const dt = new DataTransfer();
		dt.setData('text/plain', 'pasted-text');
		ta.dispatchEvent(new ClipboardEvent('paste', {clipboardData: dt, bubbles: true, cancelable: true}));
		return true;
	})()`, r.n), nil)
	// More queries while the terminal has the focus.
	if err := r.c.Send(context.Background(), r.id, "k2", control.Input{Text: "screen " + terminalQueries + "second-query-marker\nreply two"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	r.untilScreen("the second queries", func(s string) bool { return strings.Contains(s, "second-query-marker") })
	eventually(t, "the second turn to end", 15*time.Second, func() bool { x := r.info(); return x.Turns == 2 && x.State == control.Idle })
	time.Sleep(time.Second) // anything the page would send has time to go
	b.eval(`1`, nil)        // and the browser's events are read

	// Every request of the page: a GET or HEAD of its own routes, with no
	// body, and nothing typed in its address.
	page, _ := url.Parse(r.base)
	routes := []string{"/", "/signin", "/api/session", "/favicon.ico", "/api/threads", "/api/thread", "/api/events", "/api/terminal", "/api/limits", "/api/workspace", "/api/hostterminals", "/api/hostscreen"}
	requests := 0
	for _, e := range b.events {
		switch e.Method {
		case "Network.webSocketCreated":
			t.Fatalf("the page opened a WebSocket: %s", e.Params)
		case "Network.requestWillBeSent":
			var p struct {
				Request struct {
					URL, Method string
					HasPostData bool
					PostData    string
					Headers     map[string]string
				}
			}
			json.Unmarshal(e.Params, &p)
			q := p.Request
			u, err := url.Parse(q.URL)
			if err != nil {
				t.Fatalf("request %q: %v", q.URL, err)
			}
			if u.Scheme == "data" || u.Scheme == "about" {
				continue
			}
			requests++
			if (q.Method != "GET" && q.Method != "HEAD") || q.HasPostData || q.PostData != "" {
				t.Fatalf("the page sent %s %s with a body %v %q", q.Method, q.URL, q.HasPostData, q.PostData)
			}
			if u.Scheme != page.Scheme || u.Host != page.Host {
				t.Fatalf("the page asked another origin: %s", q.URL)
			}
			if !slices.Contains(routes, u.Path) && !strings.HasPrefix(u.Path, "/assets/") {
				t.Fatalf("the page asked for %s", q.URL)
			}
			// Nothing can ride in the address or a header: each route takes
			// only its own parameters, with this thread's id or a number,
			// and the headers are those a browser sends by itself, and the
			// session, which is the server's own token. No cookie.
			number := regexp.MustCompile(`^[0-9]+$`)
			for name, vs := range u.Query() {
				allowed := map[string][]string{"/api/thread": {"thread", "recent", "events"},
					"/api/terminal": {"thread"}, "/api/workspace": {"thread"}, "/api/hostscreen": {"pane"}, "/api/events": {"after"}, "/signin": {"t"}}[u.Path]
				if !slices.Contains(allowed, name) || len(vs) != 1 {
					t.Fatalf("a request has a parameter %q it does not need: %s", name, q.URL)
				}
				v := vs[0]
				switch name {
				case "thread":
					if v != fmt.Sprint(int64(r.id)) {
						t.Fatalf("a request names thread %q: %s", v, q.URL)
					}
				case "recent", "events", "after":
					if !number.MatchString(v) {
						t.Fatalf("a request's %s is %q: %s", name, v, q.URL)
					}
				}
			}
			browserHeaders := []string{"accept", "accept-encoding", "accept-language", "cache-control", "pragma", "user-agent",
				"upgrade-insecure-requests", "referer", "origin", "last-event-id", "range", "if-none-match", "if-modified-since", "priority"}
			for h, v := range q.Headers {
				h = strings.ToLower(h)
				if h == "authorization" {
					if !regexp.MustCompile(`^Bearer [A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`).MatchString(v) &&
						!(u.Path == "/api/session" && regexp.MustCompile(`^Link [A-Za-z0-9_-]+$`).MatchString(v)) {
						t.Fatalf("a request's Authorization is %q", v)
					}
					continue
				}
				if strings.HasPrefix(h, "sec-") || slices.Contains(browserHeaders, h) {
					if h == "last-event-id" && !regexp.MustCompile(`^[0-9]+(:[0-9a-f]*)?$`).MatchString(v) {
						t.Fatalf("a request's Last-Event-ID is %q", v)
					}
					continue
				}
				t.Fatalf("a request has a header %s: %q (%s)", h, v, q.URL)
			}
			// The address, decoded until nothing more decodes, holds no byte
			// of what was typed and no answer to a query (each begins with
			// ESC, or is a control character).
			plain := u.Path + "?" + u.RawQuery
			for {
				d, err := url.QueryUnescape(plain)
				if err != nil || d == plain {
					break
				}
				plain = d
			}
			for _, typed := range []string{"typed", "inserted", "pasted", "[?", ";2c", "rgb:"} {
				if strings.Contains(plain, typed) {
					t.Fatalf("a request carries what was typed or answered (%q): %s", typed, q.URL)
				}
			}
			if strings.ContainsFunc(plain, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
				t.Fatalf("a request carries a control character: %s", q.URL)
			}
		}
	}
	if requests == 0 {
		t.Fatal("no request of the page was seen: the check would test nothing")
	}

	// The session got no input: two turns, both from the control socket,
	// nothing interrupted, and the server dropped no input.
	_, out, _ := cli(t, "events", "--root", r.root)
	for _, e := range lines(t, out) {
		if e["kind"] == "turn_started" && e["data"].(map[string]any)["by"] != "caller" {
			t.Fatalf("a turn the page started: %v", e)
		}
		if e["kind"] == "turn_ended" && e["data"].(map[string]any)["outcome"] != "completion" {
			t.Fatalf("a turn ended otherwise: %v", e)
		}
	}
	if x := r.info(); x.Turns != 2 {
		t.Fatalf("%d turns, want 2", x.Turns)
	}
	if strings.Contains(r.log(), "input to a terminal dropped") || strings.Contains(r.log(), "upgrade of a terminal stream dropped") {
		t.Fatalf("the server dropped input from the page:\n%s", r.log())
	}
	for _, typed := range []string{"typed-keys", "inserted-text", "pasted-text"} {
		logs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*", host.TerminalLog))
		for _, l := range logs {
			if b, _ := os.ReadFile(l); strings.Contains(string(b), typed) {
				t.Fatalf("%q reached the session's terminal", typed)
			}
		}
	}
	r.noViolations()
}

// A reset chunk clears the terminal before it draws; an asleep session
// shows its last screen and "no process".
func TestDashboardTerminalPaneResetAndAsleep(t *testing.T) {
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["idle"] = "2s" })
	r := newPaneRig(t, root, "screen first-marker\nreply one", func(x control.ThreadInfo) bool { return x.Turns == 1 })
	noProcess := `!!document.querySelector('.pane.active [data-no-process]')`
	r.untilScreen("the first run's output", func(s string) bool { return strings.Contains(s, "first-marker") })

	// Asleep: the last screen, and "no process".
	eventually(t, "the thread to fall asleep", 20*time.Second, func() bool { return r.info().State == control.Asleep })
	r.b.until(`"no process"`, noProcess+` && document.querySelector('.pane.active [data-no-process]').textContent === 'no process'`)
	var visible bool
	r.b.eval(fmt.Sprintf(`(() => { const e = document.querySelector('[data-terminal="%d"] .xterm-screen'); const b = e?.getBoundingClientRect(); return !!b && b.width > 0 && b.height > 0 && getComputedStyle(e).visibility !== 'hidden'; })()`, r.n), &visible)
	if s := r.screen(); !strings.Contains(s, "first-marker") || !visible {
		t.Fatalf("the asleep session's last screen is not shown (visible %v):\n%s", visible, s)
	}

	// Awake again: the new run starts with a reset, the end of the kept log,
	// which still holds the old run's output. Drawn on a cleared terminal it
	// shows each old line once; drawn over the old screen, twice.
	if err := r.c.Send(context.Background(), r.id, "k2", control.Input{Text: "screen second-marker\nreply two"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	s := r.untilScreen("the new run's output", func(s string) bool { return strings.Contains(s, "second-marker") })
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	kept, _ := os.ReadFile(filepath.Join(dirs[0], host.TerminalLog))
	if strings.Count(string(kept), "first-marker") != 1 {
		t.Fatalf("the kept log holds the old output %d times, want once: the check would test nothing", strings.Count(string(kept), "first-marker"))
	}
	if n := strings.Count(s, "first-marker"); n != 1 {
		t.Fatalf("after the reset the old line is on the screen %d times, want once:\n%s", n, s)
	}
	r.b.until(`no "no process" once the session runs`, `!document.querySelector('.pane.active [data-no-process]')`)
	r.noViolations()
}

// A session without a terminal shows only its conversation (#336): its
// entries in order, those before the tab opened and those after.
func TestDashboardEventsPane(t *testing.T) {
	root := newRoot(t) // the fake agent on pipes: no terminal
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	r := newPaneRig(t, root, "tool quick 50ms\nreply alpha", func(x control.ThreadInfo) bool { return x.State == control.Idle && x.Turns == 1 })
	conv := fmt.Sprintf(`.pane.active [data-conversation="%d"]`, r.n)
	r.b.until("the conversation, and no terminal", fmt.Sprintf(`!!document.querySelector('%s') && !document.querySelector('.pane.active .xterm') && !document.querySelector('.pane.active .view-switch')`, conv))
	if n := len(r.b.untilConversation("the events before the tab opened", conv, r.c, r.id)); n < 5 {
		t.Fatalf("only %d entries shown", n)
	}
	if err := r.c.Send(context.Background(), r.id, "k2", control.Input{Text: "tool slow 100ms\nreply beta"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second turn to end", 15*time.Second, func() bool { x := r.info(); return x.Turns == 2 && x.State == control.Idle })
	r.b.untilConversation("the events of a turn after the tab opened", conv, r.c, r.id)
	r.noViolations()
}
