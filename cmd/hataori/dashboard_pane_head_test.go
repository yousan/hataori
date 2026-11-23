package main

import (
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// paneHead is what the active pane's header shows: the state label, and
// the Stop button set against it.
type paneHead struct {
	State    string // the label's data-pane-state
	Text     string // the label's words
	Labels   int    // state labels in the pane
	Stops    int    // Stop buttons in the pane
	Apart    bool   // the label and the Stop button are distinct elements, neither inside the other
	StopText string
	// The Stop button's colours that match a state's colour.
	StateColoured []string
}

const paneHeadJS = `(() => {
	const p = document.querySelector('.pane.active');
	const label = p?.querySelector('[data-pane-state]');
	const stop = p?.querySelector('[data-stop-button]');
	const rgb = (c) => { const e = document.createElement('span'); e.style.color = c; document.body.append(e); const v = getComputedStyle(e).color; e.remove(); return v; };
	const root = getComputedStyle(document.documentElement);
	const states = ['creating', 'idle', 'running', 'waiting', 'asleep', 'closed', 'removed', 'failed'].map(s => rgb(root.getPropertyValue('--st-' + s).trim()));
	const coloured = [];
	if (stop) {
		const st = getComputedStyle(stop);
		for (const [k, v] of [['color', st.color], ['background', st.backgroundColor], ['border', st.borderTopColor]]) if (states.includes(v)) coloured.push(k + ' ' + v);
	}
	return {
		state: label?.dataset.paneState ?? '',
		text: label?.textContent ?? '',
		labels: p ? p.querySelectorAll('[data-pane-state]').length : 0,
		stops: p ? p.querySelectorAll('[data-stop-button]').length : 0,
		apart: !!label && !!stop && label !== stop && !label.contains(stop) && !stop.contains(label),
		stopText: stop?.textContent ?? '',
		stateColoured: coloured,
	};
})()`

func (b *browser) paneHead() paneHead {
	b.t.Helper()
	var h paneHead
	b.eval(paneHeadJS, &h)
	return h
}

// The acceptance criteria of #683 for the header, in the browser against
// the daemon with the fake surface and the fake agent: the pane's header
// shows the thread's state in words with its age; while a turn runs it
// also has Stop, an element apart from the state, labelled as an action
// and in no state's colour; with no turn running there is no Stop.
func TestDashboardPaneHeadStateAndStop(t *testing.T) {
	r := boxRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	x := threadOf(t, r.root)
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, x.ID)
	b.until("the state in the header", `document.querySelector('.pane.active .pane-head [data-pane-state]')?.dataset.paneState === 'idle'`)
	time.Sleep(500 * time.Millisecond)
	words := regexp.MustCompile(`^(idle|running) · \d+[smhd]$`)
	if h := b.paneHead(); h.Labels != 1 || !words.MatchString(h.Text) || !strings.HasPrefix(h.Text, "idle") || h.Stops != 0 {
		t.Fatalf("a thread that does not run: %+v; want one label \"idle · <age>\" and no Stop", h)
	}

	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "tool wait 60s\nreply late")
	b.until("the Stop button of a running turn", `!!document.querySelector('.pane.active [data-stop-button]')`)
	b.until("the state running", `document.querySelector('.pane.active [data-pane-state]')?.dataset.paneState === 'running'`)
	h := b.paneHead()
	if h.Labels != 1 || h.Stops != 1 || !h.Apart || !strings.HasPrefix(h.Text, "running") || !words.MatchString(h.Text) {
		t.Fatalf("a running thread: %+v; want the label \"running · <age>\" and Stop as distinct elements", h)
	}
	if h.StopText != "Stop this turn" || len(h.StateColoured) != 0 {
		t.Fatalf("Stop reads %q in %v; want \"Stop this turn\" in no state's colour", h.StopText, h.StateColoured)
	}
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Stop(t.Context(), x.ID, 0); err != nil {
		t.Fatal(err)
	}
	b.until("no Stop once the turn ended", `!document.querySelector('.pane.active [data-stop-button]') && document.querySelector('.pane.active [data-pane-state]')?.dataset.paneState !== 'running'`)
	if h := b.paneHead(); h.Labels != 1 || h.Stops != 0 {
		t.Fatalf("after the turn: %+v; want the label and no Stop", h)
	}
}

// The acceptance criteria of #683 for typing on the web listener, in the
// browser against the daemon with the fake surface and the fake agent in
// a terminal: Type is labelled as an action; a key, and a click, in the
// terminal while it is off shows the hint over the terminal and sends
// nothing, and makes no passkey check; the hint's button starts the check
// and attaches.
func TestDashboardTypeHintOnTheWeb(t *testing.T) {
	d := newPasskeyDash(t)
	setConfig(t, d.root, func(cfg map[string]any) {
		ad := cfg["adapters"].(map[string]any)[fake.Name].(map[string]any)
		ad["terminal"], ad["hataori"] = true, os.Args[0] // its hooks run as this binary
	})
	logged := startDaemonLog(t, d.root)
	c, err := wire.Dial(d.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	startThread(t, d.root, "t1", "screen ready-for-keys")
	var x control.ThreadInfo
	eventually(t, "the thread", 20*time.Second, func() bool {
		ts, _ := c.Threads(t.Context(), control.Filter{})
		if len(ts) == 1 {
			x = ts[0]
		}
		return len(ts) == 1 && x.State == control.Idle
	})
	b := newBrowser(t, "--host-resolver-rules=MAP "+webName+" 127.0.0.1",
		"--ignore-certificate-errors-spki-list="+base64.StdEncoding.EncodeToString(d.spki))
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("WebAuthn.enable", map[string]any{})
	b.call("WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{
		"protocol": "ctap2", "transport": "internal", "hasResidentKey": true, "hasUserVerification": true,
		"isUserVerified": true, "automaticPresenceSimulation": true}})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, d.root, "--web")})
	b.until("the offer to add a passkey", `!!document.querySelector('[data-passkey=offer] [data-passkey=register]')`)
	b.press("[data-passkey=register]")
	b.until("the passkey added", `document.querySelector('[data-passkey=offer]')?.textContent.includes('Passkey added')`)
	from := len(b.events)
	openThreadTerminal(t, b, x.ID, "ready-for-keys")
	b.until("Type, off", `document.querySelector('.pane.active .attach')?.dataset.attach === 'off'`)
	var label string
	b.eval(`document.querySelector('.pane.active .attach [role=switch]').textContent`, &label)
	if label != "Type into the terminal" {
		t.Fatalf("Type reads %q, want \"Type into the terminal\"", label)
	}
	if n := countOf(b, ".pane.active [data-type-hint]"); n != 0 {
		t.Fatalf("%d hints before a key", n)
	}
	nothingSent := func(what string) {
		t.Helper()
		time.Sleep(500 * time.Millisecond)
		if keys, other := b.keysSent(from); keys != "" || len(other) != 0 {
			t.Fatalf("%s: the page sent the keys %q and the frames %q", what, keys, other)
		}
		if strings.Contains(logged(), `msg="dashboard: terminal attached"`) {
			t.Fatalf("%s: an attachment started", what)
		}
		if keys := d.stored(); len(keys) != 1 || keys[0].Used != nil {
			t.Fatalf("%s: a passkey check was made: %+v", what, keys)
		}
	}

	// A key with Type off.
	focusTerminal(b)
	b.key("a", "KeyA", 65, 0, "a")
	b.until("the hint over the terminal", `(document.querySelector('.pane.active .terminal-view [data-type-hint]')?.textContent ?? '') === 'To type here, turn on input (passkey check)'`)
	nothingSent("a key with Type off")

	// A click with Type off, once the hint went with the view.
	b.click(".pane.active .view-switch [data-view=conversation]")
	b.click(".pane.active .view-switch [data-view=terminal]")
	b.until("the terminal again, without the hint", `!!document.querySelector('.pane.active .terminal-view .xterm-screen') && !document.querySelector('.pane.active [data-type-hint]')`)
	var at struct{ X, Y float64 }
	b.eval(`(() => { const r = document.querySelector('.pane.active .terminal-view .xterm-screen').getBoundingClientRect(); return {x: r.left + 20, y: r.top + 40}; })()`, &at)
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		b.call("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": at.X, "y": at.Y, "button": "left", "clickCount": 1})
	}
	b.until("the hint after a click", `!!document.querySelector('.pane.active .terminal-view [data-type-hint]')`)
	nothingSent("a click with Type off")

	// The hint's button starts the same check as Type.
	b.press(".pane.active [data-type-hint-on]")
	b.until("attached after the check", `document.querySelector('.pane.active .attach')?.dataset.attach === 'on' && document.querySelector('.pane.active .terminal-view').dataset.typing === 'on'`)
	if keys := d.stored(); len(keys) != 1 || keys[0].Used == nil {
		t.Fatalf("the hint's button made no passkey check: %+v", keys)
	}
	if n := countOf(b, ".pane.active [data-type-hint]"); n != 0 {
		t.Fatalf("%d hints while typing", n)
	}
}
