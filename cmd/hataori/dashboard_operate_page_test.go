package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// operatePage opens the dashboard of the rig's daemon at link in a browser
// that records its requests and WebSocket frames, and waits for its
// thread.
func (r *operateRig) operatePage(t *testing.T, link string, flags ...string) *browser {
	t.Helper()
	b := newBrowser(t, flags...)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the thread in the tree", `!!document.querySelector('[data-section=threads] .row')`)
	return b
}

// paneTab is the selector of the rig's pane's tab.
func (r *operateRig) paneTab() string {
	return fmt.Sprintf(`.tab[data-tab="host:%s#%s"]`, r.device, r.pane)
}

// active is the selector of sel inside the active pane.
func active(sel string) string { return ".pane.active " + sel }

// openPane opens the rig's pane from the tree.
func (r *operateRig) openPane(b *browser) {
	b.t.Helper()
	b.unfoldHosts()
	b.until("the pane in the tree", fmt.Sprintf(`!!document.querySelector('.tree [data-host=%q]')`, r.device))
	b.click(fmt.Sprintf(`.tree [data-host=%q]`, r.device))
	b.until("the pane's screen", fmt.Sprintf(`!!document.querySelector(%q)`, active(fmt.Sprintf("[data-host-screen=%q] .xterm", r.pane))))
}

// switchState is the switch's state in the active pane: "", "off",
// "starting", "on" or "ended", and why it ended.
func (b *browser) switchState() (string, string) {
	b.t.Helper()
	var v struct{ Phase, Why string }
	b.eval(`(() => { const o = document.querySelector('.pane.active [data-operate]'); return {phase: o ? o.dataset.operate : '', why: o?.querySelector('[data-operate-end]')?.dataset.operateEnd ?? ''}; })()`, &v)
	return v.Phase, v.Why
}

// focusPane puts the keyboard's focus in the active pane's terminal.
func (r *operateRig) focusPane(b *browser) {
	b.t.Helper()
	b.eval(fmt.Sprintf(`document.querySelector(%q).focus(), true`, active(fmt.Sprintf("[data-host-screen=%q] textarea", r.pane))), nil)
}

// key presses one key, as the keyboard sends it.
func (b *browser) key(key, code string, vk, modifiers int, text string) {
	b.t.Helper()
	down := map[string]any{"type": "keyDown", "key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": modifiers}
	if text != "" {
		down["text"], down["unmodifiedText"] = text, text
	}
	b.call("Input.dispatchKeyEvent", down)
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": code, "windowsVirtualKeyCode": vk, "modifiers": modifiers})
}

// sentFrames are the WebSocket frames the page sent, from event from on.
func (b *browser) sentFrames(from int) []string {
	b.t.Helper()
	b.eval(`1`, nil) // read the events so far
	var out []string
	for _, e := range b.events[from:] {
		if e.Method != "Network.webSocketFrameSent" {
			continue
		}
		var p struct{ Response struct{ PayloadData string } }
		json.Unmarshal(e.Params, &p)
		out = append(out, p.Response.PayloadData)
	}
	return out
}

// endedWithin waits until the active pane says the operation ended for
// why and its terminal is read-only again, and fails when that came more
// than a second after the daemon logged the end.
func (r *operateRig) endedWithin(t *testing.T, b *browser, why string, endsBefore int) {
	t.Helper()
	var logged, shown time.Time
	for deadline := time.Now().Add(30 * time.Second); logged.IsZero() || shown.IsZero(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			phase, w := b.switchState()
			t.Fatalf("the end (%s) logged at %v, shown at %v; the switch is %s %s\n%s", why, logged, shown, phase, w, r.logged())
		}
		if logged.IsZero() && strings.Count(r.logged(), `msg="host input ended"`) > endsBefore {
			logged = time.Now()
		}
		if shown.IsZero() {
			var ok bool
			b.eval(fmt.Sprintf(`(() => { const o = document.querySelector('.pane.active [data-operate]');
				return o?.dataset.operate === 'ended' && o.querySelector('[data-operate-end]')?.dataset.operateEnd === %q &&
					document.querySelector('.pane.active [data-host-screen]')?.dataset.operating === 'off' &&
					document.querySelector('.pane.active [role=switch]')?.getAttribute('aria-checked') === 'false'; })()`, why), &ok)
			if ok {
				shown = time.Now()
			}
		}
	}
	if d := shown.Sub(logged); d > time.Second {
		t.Errorf("the page showed the end (%s) %v after the daemon logged it", why, d)
	}
}

// ends counts the daemon's end lines.
func (r *operateRig) ends() int { return strings.Count(r.logged(), `msg="host input ended"`) }

// On the loopback listener, in the browser: a thread's tab has no switch;
// the pane's tab has one, off when it opens; switched on, the operation
// starts at once, the tab and the status bar show the pane and the time
// left, and the cursor is drawn where tmux has it; what is typed reaches
// the pane through the key table, and a paste as a paste, and the page
// sends nothing else; switched off, the tab is read-only again within a
// second and says so; closing the tab ends the operation.
func TestDashboardOperateInThePage(t *testing.T) {
	r := newOperateRig(t, "5m")
	startThread(t, r.root, "t1", "reply from a thread")
	b := r.operatePage(t, signinLink(t, r.root))

	// A thread's tab: no switch.
	b.click(`[data-section=threads] .row`)
	b.until("the thread's tab", `!!document.querySelector('.tab.active[data-tab^="thread:"]')`)
	var n int
	b.eval(`document.querySelectorAll('.pane.active [data-operate], .pane.active [role=switch]').length`, &n)
	if n != 0 {
		t.Fatal("a thread's tab has an operate switch")
	}

	// The pane's tab: the switch, off.
	r.openPane(b)
	b.until("the switch, off", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'off' && document.querySelector('.pane.active [role=switch]')?.getAttribute('aria-checked') === 'false'`)
	var shown string
	b.eval(`document.querySelector('.pane.active [role=switch]').textContent`, &shown)
	if shown != "Operate" {
		t.Errorf("the switch says %q", shown)
	}

	// Switched on: at once on loopback; the tab, the status bar, the time
	// left, the cursor.
	from := len(b.events)
	b.press(active("[role=switch]"))
	b.until("the operation on", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
	b.until("the tab and the status bar", fmt.Sprintf(`!!document.querySelector(%q) && !!document.querySelector('[data-status-operating=%q]')`, r.paneTab()+fmt.Sprintf(" [data-tab-operating=%q]", r.pane), r.pane))
	var left int
	b.eval(`+document.querySelector('.pane.active [data-operating-left]').dataset.operatingLeft`, &left)
	if left < 295 || left > 300 {
		t.Errorf("time left %d s, want about 300", left)
	}
	var texts []string
	b.eval(fmt.Sprintf(`[document.querySelector(%q).textContent, document.querySelector('[data-status-operating]').textContent]`, r.paneTab()+" [data-tab-operating]"), &texts)
	if !strings.Contains(texts[0], "4:5") && !strings.Contains(texts[0], "5:00") || !strings.Contains(texts[1], r.pane) {
		t.Errorf("the tab says %q, the status bar %q", texts[0], texts[1])
	}
	b.until("the cursor where tmux has it", fmt.Sprintf(`document.querySelector(%q)?.dataset.cursor === '7,0'`, active("[data-host-screen]")))
	if c := r.tmux("display-message", "-p", "-t", r.pane, "#{cursor_x},#{cursor_y}"); c != "7,0" {
		t.Fatalf("tmux has the cursor at %s", c)
	}

	// Typed: text, Enter, Up, Ctrl+C, Alt+x, Shift+F5; Ctrl+Tab,
	// Shift+PageUp, Alt+PageDown and Ctrl+Backspace, whose modifiers the
	// terminal emulator alone would drop; and a paste.
	r.focusPane(b)
	b.call("Input.insertText", map[string]any{"text": operateMarker})
	b.key("Enter", "Enter", 13, 0, "\r")
	b.key("ArrowUp", "ArrowUp", 38, 0, "")
	b.key("c", "KeyC", 67, 2, "")
	b.key("x", "KeyX", 88, 1, "")
	b.key("F5", "F5", 116, 8, "")
	b.key("Tab", "Tab", 9, 2, "")
	b.key("PageUp", "PageUp", 33, 8, "")
	b.key("PageDown", "PageDown", 34, 1, "")
	b.key("Backspace", "Backspace", 8, 2, "")
	b.eval(fmt.Sprintf(`(() => { const ta = document.querySelector(%q); const dt = new DataTransfer(); dt.setData('text/plain', 'pasted\ntext');
		ta.dispatchEvent(new ClipboardEvent('paste', {clipboardData: dt, bubbles: true, cancelable: true})); return true; })()`, active(fmt.Sprintf("[data-host-screen=%q] textarea", r.pane))), nil)
	want := operateMarker + "\r\x1b[A\x03\x1bx\x1b[15;2~\t\x1b[5;2~\x1b[6;3~\x08pasted\rtext"
	var got []byte
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got, _ = os.ReadFile(r.got)
		if len(got) >= len(want) || time.Now().After(deadline) {
			break
		}
	}
	if string(got) != want {
		t.Fatalf("the pane read %q, want %q", got, want)
	}

	// Nothing else left the page: the session first, then keys.
	frames := b.sentFrames(from)
	if len(frames) < 2 {
		t.Fatalf("frames sent: %q", frames)
	}
	var firstFrame struct{ Session, Pane string }
	if json.Unmarshal([]byte(frames[0]), &firstFrame) != nil || firstFrame.Session == "" || firstFrame.Pane != r.pane {
		t.Errorf("the first frame: %s", frames[0])
	}
	var keys []string
	for _, f := range frames[1:] {
		var m struct {
			Keys []control.HostKey
			End  bool
		}
		if err := json.Unmarshal([]byte(f), &m); err != nil || len(m.Keys) == 0 && !m.End {
			t.Errorf("the page sent %q", f)
		}
		for _, k := range m.Keys {
			switch {
			case k.Key != "" && (k.Text != "" || k.Paste || !control.ValidHostKey(k.Key)):
				t.Errorf("the page sent the key %+v", k)
			case k.Key != "":
				keys = append(keys, k.Key)
			case k.Paste:
				keys = append(keys, "paste:"+k.Text)
			default:
				keys = append(keys, "text:"+k.Text)
			}
		}
	}
	if w := []string{"text:" + operateMarker, "Enter", "Up", "C-c", "M-x", "S-F5", "C-Tab", "S-PPage", "M-NPage", "C-h", "paste:pasted\ntext"}; strings.Join(keys, "|") != strings.Join(w, "|") {
		t.Errorf("keys sent %q, want %q", keys, w)
	}

	// Away to another tab and back while on: the pane's terminal, made
	// again, still takes keys.
	had, _ := os.ReadFile(r.got) // the pane's cat writes on after what it wrote
	b.click(`.tab[data-tab=all] .tab-main`)
	b.until("the All threads tab", `!!document.querySelector('.tab.active[data-tab=all]')`)
	b.click(r.paneTab() + " .tab-main")
	b.until("the pane's tab again, still on", fmt.Sprintf(`document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on' && !!document.querySelector(%q)`, active(fmt.Sprintf("[data-host-screen=%q] textarea", r.pane))))
	r.focusPane(b)
	b.call("Input.insertText", map[string]any{"text": "again"})
	eventually(t, "the pane to read what was typed after the tab came back", 10*time.Second, func() bool {
		got, _ := os.ReadFile(r.got)
		return string(got) == string(had)+"again"
	})

	// Switched off: read-only within a second, and it says so.
	ends := r.ends()
	start := time.Now()
	b.press(active("[role=switch]"))
	r.endedWithin(t, b, "ended", ends)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("switched off after %v", d)
	}
	// The server ended it because the page asked, not because the page
	// went away.
	if log := r.logged(); !strings.HasSuffix(strings.TrimSpace(log[strings.LastIndex(log, `msg="host input ended"`):]), "reason=ended") {
		t.Errorf("the end after switching off:\n%s", log)
	}
	var says string
	b.eval(`document.querySelector('.pane.active [data-operate-end]').textContent`, &says)
	if says != "Typing switched off." {
		t.Errorf("it says %q", says)
	}
	b.until("no operating mark", fmt.Sprintf(`!document.querySelector('[data-tab-operating]') && !document.querySelector('[data-status-operating]') && document.querySelector(%q)?.dataset.cursor === ''`, active("[data-host-screen]")))

	// Closing the tab ends the operation; the tab opened again is off.
	b.press(active("[role=switch]"))
	b.until("the operation on again", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
	ends = r.ends()
	b.click(r.paneTab() + " .tab-close")
	eventually(t, "the end's log line", 2*time.Second, func() bool { return r.ends() > ends })
	if !strings.HasSuffix(strings.TrimSpace(r.logged()[strings.LastIndex(r.logged(), `msg="host input ended"`):]), "reason=closed") {
		t.Errorf("the end after the tab closed:\n%s", r.logged())
	}
	r.openPane(b)
	b.until("the switch, off again", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'off' && !document.querySelector('.pane.active [data-operate-end]')`)

	// A second click while it starts switches it off: the switch shows
	// "starting", not off, so it never starts twice.
	starts := strings.Count(r.logged(), `msg="host input started"`)
	b.eval(`(() => { const s = document.querySelector('.pane.active [role=switch]'); s.click(); s.click(); return true; })()`, nil)
	b.until("switched off while starting", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'ended' && document.querySelector('.pane.active [data-operate-end]')?.dataset.operateEnd === 'ended'`)
	time.Sleep(time.Second)
	if phase, _ := b.switchState(); phase != "ended" {
		t.Errorf("after two clicks the switch is %s", phase)
	}
	if n := strings.Count(r.logged(), `msg="host input started"`) - starts; n > 1 {
		t.Errorf("two clicks started %d operations", n)
	}
	if strings.Count(r.logged(), `msg="host input started"`) != r.ends() {
		t.Errorf("an operation is left open after two clicks:\n%s", r.logged())
	}

	// Closing the browser's tab ends it too.
	b.press(active("[role=switch]"))
	b.until("the operation on again", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
	ends = r.ends()
	b.call("Page.navigate", map[string]any{"url": "about:blank"})
	eventually(t, "the end once the page is left", 3*time.Second, func() bool { return r.ends() > ends })

	if strings.Contains(r.logged(), operateMarker) {
		t.Error("the daemon's log holds what was typed")
	}
}

// An operation that ends by itself, after the idle period and when the
// pane goes, makes the tab read-only again within a second, and says why.
func TestDashboardOperateEndsInThePage(t *testing.T) {
	r := newOperateRig(t, "2s")
	startThread(t, r.root, "t1", "reply from a thread")
	b := r.operatePage(t, signinLink(t, r.root))
	r.openPane(b)
	b.until("the switch", `!!document.querySelector('.pane.active [role=switch]')`)

	ends := r.ends()
	b.press(active("[role=switch]"))
	b.until("the operation on", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
	r.endedWithin(t, b, "idle", ends)

	ends = r.ends()
	b.press(active("[role=switch]"))
	b.until("the operation on again", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
	r.tmux("new-window", "-d", "-t", "work:", "exec sleep 600") // the server stays
	r.tmux("kill-pane", "-t", r.pane)
	r.endedWithin(t, b, "pane gone", ends)
}

// With host input off, a host pane's tab shows no switch: the server says
// the person may not operate.
func TestDashboardOperateSwitchNeedsHostInput(t *testing.T) {
	r := newOperateRigWith(t, "5m", false)
	startThread(t, r.root, "t1", "reply from a thread")
	b := r.operatePage(t, signinLink(t, r.root))
	r.openPane(b)
	time.Sleep(time.Second) // the page has asked the server by now
	var n int
	b.eval(`document.querySelectorAll('.pane.active [data-operate], .pane.active [role=switch]').length`, &n)
	if n != 0 {
		t.Fatal("the switch shows with host input off")
	}
}

// On the web listener, in a browser with a virtual authenticator: a
// person with a passkey switches on and is asked for a passkey check, and
// the operation starts with it; a person with no passkey whose sign-in is
// more than five minutes old is told to sign in again, and a fresh sign-in
// starts at once.
func TestDashboardOperateOnTheWebInThePage(t *testing.T) {
	t.Run("a passkey", func(t *testing.T) {
		r := newOperateRig(t, "5m")
		startThread(t, r.root, "t1", "reply from a thread")
		b := r.operatePage(t, signinLink(t, r.root, "--web"), "--host-resolver-rules=MAP "+webName+" 127.0.0.1",
			"--ignore-certificate-errors-spki-list="+base64.StdEncoding.EncodeToString(r.spki))
		b.call("WebAuthn.enable", map[string]any{})
		var va struct {
			AuthenticatorID string `json:"authenticatorId"`
		}
		json.Unmarshal(b.call("WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{
			"protocol": "ctap2", "transport": "internal", "hasResidentKey": true, "hasUserVerification": true,
			"isUserVerified": true, "automaticPresenceSimulation": true}}), &va)
		b.until("the offer to add a passkey", `!!document.querySelector('[data-passkey=offer] [data-passkey=register]')`)
		b.press("[data-passkey=register]")
		b.until("the passkey added", `document.querySelector('[data-passkey=offer]')?.textContent.includes('Passkey added')`)
		if keys := r.stored(); len(keys) != 1 || keys[0].Used != nil {
			t.Fatalf("passkeys %+v", keys)
		}

		r.openPane(b)
		b.until("the switch", `!!document.querySelector('.pane.active [role=switch]')`)
		b.press(active("[role=switch]"))
		b.until("the operation on, after the passkey check", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
		if keys := r.stored(); len(keys) != 1 || keys[0].Used == nil {
			t.Errorf("no passkey check was made: %+v", keys)
		}
		r.focusPane(b)
		b.call("Input.insertText", map[string]any{"text": operateMarker})
		eventually(t, "the pane to read what was typed", 10*time.Second, func() bool {
			got, _ := os.ReadFile(r.got)
			return string(got) == operateMarker
		})

		// Without a passkey check that answers, nothing starts.
		b.press(active("[role=switch]"))
		b.until("switched off", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'ended'`)
		b.call("WebAuthn.setUserVerified", map[string]any{"authenticatorId": va.AuthenticatorID, "isUserVerified": false})
		b.press(active("[role=switch]"))
		b.until("the start refused", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'ended' && document.querySelector('.pane.active [data-operate-end]')?.dataset.operateEnd !== 'ended'`)
		if phase, why := b.switchState(); phase != "ended" || why == "" {
			t.Errorf("without a verified check: %s %s", phase, why)
		}
		if strings.Contains(r.logged(), operateMarker) {
			t.Error("the daemon's log holds what was typed")
		}
	})

	t.Run("no passkey", func(t *testing.T) {
		clock := useClock(t)
		r := newOperateRig(t, "5m")
		clock.add(2 * time.Second) // a sign-in in the second the server started counts as before it
		startThread(t, r.root, "t1", "reply from a thread")
		flags := []string{"--host-resolver-rules=MAP " + webName + " 127.0.0.1",
			"--ignore-certificate-errors-spki-list=" + base64.StdEncoding.EncodeToString(r.spki)}
		b := r.operatePage(t, signinLink(t, r.root, "--web"), flags...)
		r.openPane(b)
		b.until("the switch", `!!document.querySelector('.pane.active [role=switch]')`)
		clock.add(5*time.Minute + time.Second)
		b.press(active("[role=switch]"))
		b.until("told to sign in again", `document.querySelector('.pane.active [data-operate-end]')?.dataset.operateEnd === 'signIn'`)
		var says string
		b.eval(`document.querySelector('.pane.active [data-operate-end]').textContent`, &says)
		if !strings.Contains(says, "open a new sign-in link") {
			t.Errorf("it says %q", says)
		}
		if n := strings.Count(r.logged(), `msg="host input started"`); n != 0 {
			t.Errorf("%d operations started", n)
		}

		b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root, "--web")})
		b.until("the thread in the tree", `!!document.querySelector('[data-section=threads] .row')`)
		r.openPane(b)
		b.until("the switch", `!!document.querySelector('.pane.active [role=switch]')`)
		b.press(active("[role=switch]"))
		b.until("the operation on, with a fresh sign-in", `document.querySelector('.pane.active [data-operate]')?.dataset.operate === 'on'`)
	})
}
