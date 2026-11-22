package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/wire"
)

// hostPage starts the daemon with host terminals on, reading the rig's own
// tmux server, and opens the dashboard in a browser that records its
// requests and any Content-Security-Policy violation.
func hostPage(t *testing.T, rig hostTerminalRig, on bool) (*browser, *wire.Client, string, func() string) {
	t.Helper()
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = rig.socket }
	t.Cleanup(func() { hostTermsHook = nil })
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = on })
	_, logged := runDaemonLog(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread", 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread in the tree", `!!document.querySelector('[data-section=threads] .row')`)
	return b, c, link, logged
}

// noStateFor fails when anything near the host terminal device shows a
// state: a mark, a dot, a badge, a state attribute.
func (b *browser) noStateFor(device, what string) {
	b.t.Helper()
	var n int
	b.eval(fmt.Sprintf(`(() => {
		const els = [...document.querySelectorAll('[data-host=%[1]q], .tab[data-tab^="host:%[1]s"], [data-host-details=%[1]q], [data-host-meta=%[1]q]')];
		return els.reduce((n, e) => n + e.querySelectorAll('.agent-mark, .dot, .badge, [data-badge], [data-state], .state-text, .flag, .why').length + (e.matches('[data-state]') ? 1 : 0), 0);
	})()`, device), &n)
	if n != 0 {
		b.t.Fatalf("%s: %d state marks shown for a terminal hataori does not own", what, n)
	}
}

// The tree's tmux and other-terminal sections: absent when the server
// reports the feature off; a tmux pane opened from the tree shows its
// screen and details and no state; a terminal outside tmux shows its
// metadata only; the page sends nothing to these terminals; and on a
// phone the same sections are in the list, without sideways scroll.
func TestDashboardHostTerminalsInThePage(t *testing.T) {
	rig := newHostTerminalRig(t)

	t.Run("off", func(t *testing.T) {
		b, _, _, _ := hostPage(t, rig, false)
		// The page asked the server, and the server said no.
		var refused bool
		for deadline := time.Now().Add(10 * time.Second); !refused && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			b.eval(`1`, nil)
			for _, e := range b.events {
				if e.Method != "Network.responseReceived" {
					continue
				}
				var p struct {
					Response struct {
						URL    string
						Status int
					}
				}
				json.Unmarshal(e.Params, &p)
				if strings.HasSuffix(p.Response.URL, "/api/hostterminals") && p.Response.Status == 403 {
					refused = true
				}
			}
		}
		if !refused {
			t.Fatal("the page never heard that host terminals are off: the check would test nothing")
		}
		time.Sleep(300 * time.Millisecond)
		var n int
		b.eval(`document.querySelectorAll('[data-section=tmux], [data-section=others], [data-host]').length`, &n)
		if n != 0 {
			t.Fatalf("with the feature off the page shows %d host terminal elements", n)
		}
	})

	b, c, link, logged := hostPage(t, rig, true)
	ctx := context.Background()
	hostRow := func(dev string) string { return fmt.Sprintf(`[data-host=%q]`, dev) }

	// 1. On: both sections, with the pane under its session and the other
	// terminal; no state for either. They are under the host terminals'
	// node, folded at first (#405).
	b.unfoldHosts()
	b.until("the pane and the other terminal in their sections", fmt.Sprintf(
		`!!document.querySelector('.tree [data-node=host] [data-session=work] %s') && !!document.querySelector('.tree [data-node=host] [data-section=others] %s')`, hostRow(rig.device), hostRow(rig.outside)))
	b.noStateFor(rig.device, "the pane's row")
	b.noStateFor(rig.outside, "the other terminal's row")

	// 2. The pane opened from the tree: its screen and its details.
	b.click(`.tree ` + hostRow(rig.device))
	// The screen, with a line of wide characters on one row.
	b.until("the pane's screen", fmt.Sprintf(`[...document.querySelectorAll('.pane.active [data-host-screen=%q] .xterm-rows > div')].some(d => d.textContent.replace(/\s+/g, ' ').includes('pane-marker 幅の広い文字'))`, rig.pane))
	var details map[string]string
	b.eval(fmt.Sprintf(`Object.fromEntries([...document.querySelectorAll('.details [data-host-details=%q] [data-field]')].map(f => [f.dataset.field, (f.querySelector('dd') ?? f).textContent]))`, rig.device), &details)
	if details["session"] != "work" || details["window"] != "0: editor" || details["device"] != rig.device || !strings.Contains(details["pane"], rig.pane) ||
		!strings.Contains(details["note"], "does not judge its state") {
		t.Fatalf("the pane's details: %q", details)
	}
	b.noStateFor(rig.device, "the pane opened")
	var status string
	b.eval(`document.querySelector('.status-item').textContent`, &status)
	if strings.Contains(status, rig.device) {
		t.Fatalf("the status bar shows the pane as a thread: %q", status)
	}

	// 4. Nothing goes to the pane: keys, Ctrl+C, a paste and typed text into
	// its terminal; no request carries them, and the pane gets none.
	b.eval(fmt.Sprintf(`document.querySelector('.pane.active [data-host-screen=%q] .xterm').click(), document.querySelector('.pane.active [data-host-screen=%q] textarea').focus(), true`, rig.pane, rig.pane), nil)
	before := len(b.events)
	for _, ch := range "typed-keys" {
		s := string(ch)
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": s, "text": s, "unmodifiedText": s})
		b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": s})
	}
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "text": "\r"})
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "c", "code": "KeyC", "windowsVirtualKeyCode": 67, "modifiers": 2})
	b.call("Input.insertText", map[string]any{"text": "inserted-text"})
	b.eval(fmt.Sprintf(`(() => { const ta = document.querySelector('.pane.active [data-host-screen=%q] textarea'); const dt = new DataTransfer(); dt.setData('text/plain', 'pasted-text');
		ta.dispatchEvent(new ClipboardEvent('paste', {clipboardData: dt, bubbles: true, cancelable: true})); return true; })()`, rig.pane), nil)
	time.Sleep(2 * time.Second)
	b.eval(`1`, nil)
	page, _ := url.Parse(link)
	for _, e := range b.events[before:] {
		if e.Method == "Network.webSocketCreated" {
			t.Fatalf("the page opened a WebSocket: %s", e.Params)
		}
		if e.Method != "Network.requestWillBeSent" {
			continue
		}
		var p struct {
			Request struct {
				URL, Method string
				HasPostData bool
			}
		}
		json.Unmarshal(e.Params, &p)
		u, _ := url.Parse(p.Request.URL)
		if u.Host != page.Host || (p.Request.Method != "GET" && p.Request.Method != "HEAD") || p.Request.HasPostData {
			t.Fatalf("the page sent %s %s (body %v)", p.Request.Method, p.Request.URL, p.Request.HasPostData)
		}
		allowed := map[string][]string{"/api/hostscreen": {"pane"}, "/api/hostterminals": nil, "/api/threads": nil, "/api/scopes": nil, "/api/layout": nil, "/api/limits": nil,
			"/api/thread": {"thread", "recent", "events"}, "/api/threadevents": {"thread", "before", "limit"},
			"/api/workspace": {"thread"}, "/api/events": {"after"}, "/api/terminal": {"thread"}}
		names, ok := allowed[u.Path]
		if !ok {
			t.Fatalf("the page asked for %s", p.Request.URL)
		}
		for name, vs := range u.Query() {
			if !slices.Contains(names, name) || (name == "pane" && vs[0] != rig.pane) {
				t.Fatalf("a request carries %s=%q: %s", name, vs, p.Request.URL)
			}
		}
	}
	cmd := exec.Command("tmux", append(append([]string{}, rig.socket...), "capture-pane", "-p", "-t", rig.pane)...)
	cmd.Env = append(os.Environ(), "TMUX=")
	screen, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{"typed", "inserted", "pasted"} {
		if strings.Contains(string(screen), typed) {
			t.Fatalf("%q reached the pane:\n%s", typed, screen)
		}
	}
	// Echoed or not: the pane's command got no byte at all.
	if got, err := os.ReadFile(rig.input); err != nil || len(got) != 0 {
		t.Fatalf("the pane's command got input %q (%v)", got, err)
	}
	if strings.Contains(logged(), "dropped") {
		t.Fatalf("the server dropped input from the page:\n%s", logged())
	}

	// 3. A terminal outside tmux: its metadata only, no screen.
	b.click(`.tree ` + hostRow(rig.outside))
	b.until("the other terminal's metadata", fmt.Sprintf(`!!document.querySelector('.pane.active [data-host-meta=%q]') && !document.querySelector('.pane.active .xterm')`+
		` && document.querySelector('.pane.active [data-host-meta=%q] [data-field=device] dd')?.textContent === %q`+
		` && document.querySelector('.pane.active [data-host-meta=%q] [data-field=process] dd')?.textContent === 'sleep'`, rig.outside, rig.outside, rig.outside, rig.outside))
	b.noStateFor(rig.outside, "the other terminal opened")
	var screens int
	b.eval(`document.querySelectorAll('[data-host-screen]').length`, &screens)
	if screens != 0 {
		t.Fatalf("a terminal outside tmux shows a screen")
	}

	// On a phone: the same sections in the list, the pane opened and left,
	// no sideways scroll.
	b.phone()
	b.call("Page.reload", map[string]any{})
	b.until("the list with the host sections", fmt.Sprintf(`document.querySelector('.shell')?.dataset.layout === 'narrow' && !!document.querySelector('.narrow-list [data-section=tmux] %s')`, hostRow(rig.device)))
	b.noSideScroll("the list with host terminals")
	b.click(`.narrow-list [data-section=tmux] summary`)
	b.click(`.narrow-list ` + hostRow(rig.device))
	b.until("the pane's screen on the phone", fmt.Sprintf(`[...document.querySelectorAll('[data-narrow-host=%q] [data-host-screen] .xterm-rows > div')].some(d => d.textContent.includes('pane-marker'))`, rig.device))
	b.noSideScroll("the pane on the phone")
	b.click(`.narrow-back`)
	b.until("the list again", `!!document.querySelector('.narrow-list') && !document.querySelector('[data-narrow-host]')`)

	var v []string
	b.eval(`window.__csp`, &v)
	if len(v) > 0 {
		t.Fatalf("Content-Security-Policy violations: %q", v)
	}
	_ = c
	_ = ctx
}
