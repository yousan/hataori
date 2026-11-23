package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// keysSent is what the page sent as keys on its attachments, from event
// from on, joined, and every other frame it sent but the first of each and
// the pane's sizes (resizesSent).
func (b *browser) keysSent(from int) (keys string, other []string) {
	b.t.Helper()
	for _, f := range b.sentFrames(from) {
		var m map[string]any
		if json.Unmarshal([]byte(f), &m) != nil {
			other = append(other, f)
			continue
		}
		switch {
		case m["keys"] != nil:
			keys += m["keys"].(string)
		case m["session"] != nil && m["thread"] != nil, m["end"] == true, m["resize"] != nil:
		default:
			other = append(other, f)
		}
	}
	return keys, other
}

// openThreadTerminal opens the thread's tab in b, on its terminal, and
// waits for the terminal to show want.
func openThreadTerminal(t *testing.T, b *browser, id control.ThreadID, want string) {
	t.Helper()
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, id))
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, id))
	b.until("the thread's terminal", fmt.Sprintf(`[...document.querySelectorAll('.pane.active [data-terminal="%d"] .xterm-rows > div')].some(d => d.textContent.includes(%q))`, id, want))
}

// focusTerminal puts the keyboard's focus in the active pane's terminal.
func focusTerminal(b *browser) {
	b.t.Helper()
	b.eval(`document.querySelector('.pane.active .terminal-view textarea').focus(), true`, nil)
}

// The acceptance criteria of #608 on the loopback listener, in the browser
// against the daemon with the fake surface and the fake agent in a
// terminal: keys typed into the focused terminal reach the session's
// terminal, the attachment opening at the first key and the keys typed
// meanwhile arriving in order, and their turn is posted in chat; keys are
// encoded for the session's modes; the page answers no query and sends no
// mouse or focus report; leaving the terminal and closing the tab end the
// attachment, and the next key opens it again. How the session's terminal
// takes the pane's size is TestDashboardTerminalTakesThePaneSize's.
func TestDashboardTerminalTakesKeys(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	// The session asks for application cursor keys, asks every query a
	// terminal answers, and asks for mouse and focus reports: once with
	// another mode in the same sequence, which the page's terminal takes,
	// so that only its filter keeps the reports in.
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen "+esc+"[?1h"+terminalQueries+esc+"[?1000;1006h"+esc+"[?25;1002;1006;1004h ready-for-keys")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	eventually(t, "the session's terminal", 15*time.Second, func() bool { x = threadOf(t, r.root); return x.Terminal != "" })

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1400, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	from := len(b.events)
	openThreadTerminal(t, b, x.ID, "ready-for-keys")
	b.until("the terminal taking keys", `document.querySelector('.pane.active .terminal-view')?.dataset.typing === 'on'`)
	if n := strings.Count(logged(), `msg="dashboard: terminal attached"`); n != 0 {
		t.Fatalf("%d attachments before a key", n)
	}

	// A click on the terminal, which asks for mouse reports, then the keys.
	var at struct{ X, Y float64 }
	b.eval(`(() => { const r = document.querySelector('.pane.active .terminal-view .xterm-screen').getBoundingClientRect(); return {x: r.left + 20, y: r.top + 20}; })()`, &at)
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		b.call("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": at.X, "y": at.Y, "button": "left", "clickCount": 1})
	}
	b.call("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": at.X, "y": at.Y, "deltaX": 0, "deltaY": 120})
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "reply typed "})
	b.call("Input.insertText", map[string]any{"text": "here"})
	b.key("Enter", "Enter", 13, 0, "\r")
	r.posted(t, th, "(typed at the terminal)\ntyped here")
	if n := strings.Count(logged(), `msg="dashboard: terminal attached"`); n != 1 {
		t.Fatalf("%d attachments after the first keys, want 1", n)
	}
	b.until("the tab and the status bar marked", fmt.Sprintf(`!!document.querySelector('.tab [data-tab-attached="%d"]') && !!document.querySelector('[data-status-attached="%d"]')`, x.ID, x.ID))
	typed := "reply typed here\r"
	sentOnly := func(what string) {
		t.Helper()
		var keys string
		var other []string
		eventually(t, what, 10*time.Second, func() bool { keys, other = b.keysSent(from); return keys == typed })
		if len(other) != 0 {
			t.Fatalf("%s: the page sent the keys %q and the frames %q; want only what was typed", what, keys, other)
		}
	}
	sentOnly("the first keys, and nothing else")
	if n := countOf(b, ".pane.active [data-type-hint]"); n != 0 {
		t.Fatalf("%d hints to turn input on, on the loopback listener", n)
	}

	// While attached, the session asks every query again: nothing answers.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "screen "+terminalQueries+esc+"[?25;1002;1006;1004h asked-again")
	b.until("the queries asked again", fmt.Sprintf(`[...document.querySelectorAll('.pane.active [data-terminal="%d"] .xterm-rows > div')].some(d => d.textContent.includes('asked-again'))`, x.ID))
	time.Sleep(500 * time.Millisecond)
	sentOnly("after the queries asked again")

	// Leaving the terminal ends the attachment; the next key opens it again.
	b.eval(`document.querySelector('.filter').focus(), true`, nil)
	eventually(t, "the end when the terminal lost focus", 10*time.Second, func() bool {
		return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 1 && strings.Contains(logged(), "reason=ended")
	})
	b.until("the marks gone", `!document.querySelector('[data-tab-attached]') && !document.querySelector('[data-status-attached]')`)
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "z"})
	eventually(t, "a second attachment", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal attached"`) == 2 })
	// The window turns narrow with the focus in the terminal: the terminal
	// goes without a blur, and the attachment ends all the same.
	b.phone()
	b.until("the narrow layout", `document.querySelector('.shell')?.dataset.layout === 'narrow'`)
	eventually(t, "the end when the terminal went", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 2 })
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1400, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.until("the terminal again", `document.querySelector('.shell')?.dataset.layout === 'wide' && document.querySelector('.pane.active .terminal-view')?.dataset.typing === 'on'`)
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "q"})
	eventually(t, "a third attachment", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal attached"`) == 3 })
	// Showing another tab ends it.
	b.click(`.tab[data-tab="all"] .tab-main`)
	eventually(t, "the end when another tab was shown", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 3 })
	// Keys typed, and the terminal left before the attachment opened: they
	// are sent all the same, then it ends.
	b.click(fmt.Sprintf(`.tab[data-tab="thread:%d"] .tab-main`, x.ID))
	b.until("the terminal again", `document.querySelector('.pane.active .terminal-view')?.dataset.typing === 'on'`)
	focusTerminal(b)
	b.key("Backspace", "Backspace", 8, 0, "") // the "z" and "q" typed before
	b.key("Backspace", "Backspace", 8, 0, "")
	b.call("Input.insertText", map[string]any{"text": "reply quick"})
	b.key("Enter", "Enter", 13, 0, "\r")
	b.eval(`document.querySelector('.filter').focus(), true`, nil)
	r.posted(t, th, "(typed at the terminal)\nquick")
	eventually(t, "the fourth end", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 4 })
	typed += "zq\x7f\x7freply quick\r"

	// Up, in application cursor mode; last, since the fake agent takes the
	// Escape it starts with as its interrupt key.
	focusTerminal(b)
	b.key("ArrowUp", "ArrowUp", 38, 0, "")
	typed += esc + "OA"
	sentOnly("Up, as ESC O A")
	eventually(t, "a fifth attachment", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal attached"`) == 5 })
	// Closing the tab ends it.
	b.click(fmt.Sprintf(`.tab[data-tab="thread:%d"] .tab-close`, x.ID))
	eventually(t, "the end when the tab closed", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 5 })
	var csp []string
	b.eval(`window.__csp`, &csp)
	if len(csp) > 0 {
		t.Fatalf("CSP violations: %v", csp)
	}
}

// After the idle period without a key the attachment ends; the next key
// on loopback opens it again.
func TestDashboardTerminalAttachesAgainAfterIdle(t *testing.T) {
	r := attachRoot(t, "1h", "1s")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen ready-for-keys")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThreadTerminal(t, b, x.ID, "ready-for-keys")
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "a"})
	eventually(t, "the idle end", 10*time.Second, func() bool {
		return strings.Contains(logged(), `msg="dashboard: terminal detached"`) && strings.Contains(logged(), "reason=idle")
	})
	b.call("Input.insertText", map[string]any{"text": "b"})
	eventually(t, "a second attachment", 10*time.Second, func() bool { return strings.Count(logged(), `msg="dashboard: terminal attached"`) == 2 })
}

// A person who may not act: the terminal sends nothing, and no switch is
// shown.
func TestDashboardTerminalOfAPersonWhoMayNotAct(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen ready-for-keys")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	dashboardHook = func(c *control.Caller) { c.Principal, c.May = "grace", control.OpRead }
	t.Cleanup(func() { dashboardHook = nil })
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	from := len(b.events)
	openThreadTerminal(t, b, x.ID, "ready-for-keys")
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "reply nothing"})
	b.key("Enter", "Enter", 13, 0, "\r")
	time.Sleep(time.Second)
	var state struct {
		Typing   string
		Switches int
	}
	b.eval(`({typing: document.querySelector('.pane.active .terminal-view').dataset.typing, switches: document.querySelectorAll('.pane.active .attach, .pane.active [role=switch]').length})`, &state)
	if keys, other := b.keysSent(from); keys != "" || len(other) != 0 || state.Typing != "off" || state.Switches != 0 {
		t.Fatalf("a person who may not act: keys %q, frames %q, %+v", keys, other, state)
	}
	if strings.Contains(logged(), `msg="dashboard: terminal attached"`) {
		t.Fatal("an attachment started")
	}
}

// On the web listener the terminal takes no key until the "Type" switch is
// on; switched on, it asks for the passkey check and attaches; the tab and
// the status bar show it, and switched off it is read-only again.
func TestDashboardTerminalOnTheWebHasASwitch(t *testing.T) {
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
	b.until("the switch, off", `document.querySelector('.pane.active .attach')?.dataset.attach === 'off' && document.querySelector('.pane.active .terminal-view').dataset.typing === 'off'`)
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "ignored"})
	time.Sleep(500 * time.Millisecond)
	if keys, _ := b.keysSent(from); keys != "" {
		t.Fatalf("with the switch off the page sent %q", keys)
	}
	b.press(".pane.active .attach [role=switch]")
	b.until("attached after the check", `document.querySelector('.pane.active .attach')?.dataset.attach === 'on'`)
	if keys := d.stored(); len(keys) != 1 || keys[0].Used == nil {
		t.Fatalf("no passkey check was made: %+v", keys)
	}
	b.until("the tab and the status bar marked", fmt.Sprintf(`!!document.querySelector('.tab [data-tab-attached="%d"]') && !!document.querySelector('[data-status-attached="%d"]')`, x.ID, x.ID))
	focusTerminal(b)
	b.call("Input.insertText", map[string]any{"text": "reply from the web"})
	b.key("Enter", "Enter", 13, 0, "\r")
	eventually(t, "the typed turn", 20*time.Second, func() bool {
		evs, _ := c.ThreadEvents(t.Context(), x.ID, 0, control.MaxEvents)
		for _, e := range evs {
			if e.Kind == "turn_started" && strings.Contains(string(e.Data), `"by":"terminal"`) {
				return true
			}
		}
		return false
	})
	b.press(".pane.active .attach [role=switch]")
	b.until("switched off", `document.querySelector('.pane.active .attach')?.dataset.attach === 'ended' && document.querySelector('.pane.active .terminal-view').dataset.typing === 'off'`)
	eventually(t, "the end logged", 10*time.Second, func() bool { return strings.Contains(logged(), `msg="dashboard: terminal detached"`) })
}
