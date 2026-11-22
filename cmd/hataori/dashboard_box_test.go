package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// boxRoot is a chat root with the fake agent on pipes, as a Codex session
// has no terminal, and the dashboard on loopback.
func boxRoot(t *testing.T) *chatRoot {
	t.Helper()
	return newChatRoot(t, func(cfg map[string]any) { cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"} })
}

// openThread opens the thread's tab from the tree in b.
func openThread(t *testing.T, b *browser, id control.ThreadID) {
	t.Helper()
	b.until("the thread in the tree", fmt.Sprintf(`!!document.querySelector('[data-section=threads] .row[data-thread="%d"]')`, id))
	b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, id))
	b.until("the thread's pane", `!!document.querySelector('.pane.active .thread-pane')`)
}

// typeInBox types text into the active pane's box; a "\n" is Shift+Enter.
// It does not send.
func typeInBox(b *browser, text string) {
	b.t.Helper()
	b.eval(`document.querySelector('.input-box textarea').focus(), true`, nil)
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			b.key("Enter", "Enter", 13, 8, "\r") // Shift+Enter
		}
		b.call("Input.insertText", map[string]any{"text": line})
	}
}

// inputsOf are the data of the thread's input events.
func inputsOf(t *testing.T, c *wire.Client, id control.ThreadID) []map[string]any {
	t.Helper()
	evs, err := c.ThreadEvents(t.Context(), id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	return dataOf(evs, "input")
}

// The acceptance criteria of #610 in the browser, against the daemon with
// the fake surface and the fake agent on pipes: a thread without a
// terminal has the box in the wide layout; Enter sends and Shift+Enter
// starts a new line, and the text of several lines reaches the agent once
// and shows in the chat link as "<owner>: <text>", its input event with
// via dashboard; a send that fails keeps the text and the server's reason,
// and sending it again uses the same key, so the agent gets it once; while
// a turn runs the box says how a message is delivered; in the narrow
// layout the box has a button that sends.
func TestDashboardInputBoxSends(t *testing.T) {
	r := boxRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	x := threadOf(t, r.root)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Network.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, x.ID)
	b.until("the box", fmt.Sprintf(`!!document.querySelector('.pane.active .input-box[data-input-box="%d"]')`, x.ID))

	// Several lines; Enter sends.
	typeInBox(b, "reply line one\nsecond line")
	b.key("Enter", "Enter", 13, 0, "\r")
	r.posted(t, th, "line one")
	r.posted(t, th, "owner: reply line one\nsecond line")
	b.until("the box empty", `document.querySelector('.input-box textarea').value === ''`)
	in := inputsOf(t, c, x.ID)
	if last := in[len(in)-1]; last["text"] != "reply line one\nsecond line" || last["via"] != "dashboard" || last["principal"] != "owner" {
		t.Fatalf("the box's input event %v", last)
	}

	// A send that fails: the text and the reason stay; sent again with the
	// same key, the agent gets it once.
	b.call("Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*/api/send", "requestStage": "Request"}}})
	from := len(b.events)
	typeInBox(b, "reply once only")
	b.key("Enter", "Enter", 13, 0, "\r")
	var paused struct {
		RequestID string `json:"requestId"`
		Request   struct{ PostData string }
	}
	eventually(t, "the send held", 10*time.Second, func() bool {
		b.eval(`1`, nil)
		for _, e := range b.events[from:] {
			if e.Method == "Fetch.requestPaused" {
				json.Unmarshal(e.Params, &paused)
				return true
			}
		}
		return false
	})
	b.call("Fetch.fulfillRequest", map[string]any{"requestId": paused.RequestID, "responseCode": 503,
		"body": base64.StdEncoding.EncodeToString([]byte("busy for a moment"))})
	b.call("Fetch.disable", map[string]any{})
	b.until("the refusal shown, the text kept", `document.querySelector('[data-box-refused]')?.textContent === 'Not sent: busy for a moment' &&
		document.querySelector('.input-box textarea').value === 'reply once only'`)
	from = len(b.events)
	b.click(".input-box .box-send")
	r.posted(t, th, "once only")
	b.until("the box empty again", `document.querySelector('.input-box textarea').value === '' && !document.querySelector('[data-box-refused]')`)
	var first, again struct{ Key string }
	json.Unmarshal([]byte(paused.Request.PostData), &first)
	for _, e := range b.events[from:] {
		var p struct {
			Request struct{ URL, PostData string }
		}
		if e.Method == "Network.requestWillBeSent" && json.Unmarshal(e.Params, &p) == nil && strings.HasSuffix(p.Request.URL, "/api/send") {
			json.Unmarshal([]byte(p.Request.PostData), &again)
		}
	}
	if first.Key == "" || again.Key != first.Key {
		t.Fatalf("the key %q, then %q: want the same", first.Key, again.Key)
	}
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, d := range inputsOf(t, c, x.ID) {
		if d["text"] == "reply once only" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the agent got the message %d times", n)
	}

	// While a turn runs, the box says how a message is delivered.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "tool wait 3s\nreply late")
	b.until("the note while a turn runs", `document.querySelector('.input-box')?.dataset.boxRunning === 'yes' && !!document.querySelector('.input-box .box-note')`)

	// The narrow layout: a button sends.
	b.phone()
	b.until("the narrow list", `document.querySelector('.shell')?.dataset.layout === 'narrow'`)
	b.until("the thread in the narrow list or open", fmt.Sprintf(`!!document.querySelector('.narrow-list .row[data-thread="%d"]') || !!document.querySelector('[data-narrow-thread="%d"]')`, x.ID, x.ID))
	b.eval(fmt.Sprintf(`(() => { const r = document.querySelector('.narrow-list .row[data-thread="%d"]'); if (r) r.click(); return true; })()`, x.ID), nil)
	b.until("the box in the narrow layout", fmt.Sprintf(`!!document.querySelector('[data-narrow-thread="%d"] .input-box .box-send')`, x.ID))
	r.posted(t, th, "late")
	typeInBox(b, "reply from the phone")
	b.click(".input-box .box-send")
	r.posted(t, th, "from the phone")
	r.posted(t, th, "owner: reply from the phone")
}

// A terminal thread has no box in the wide layout, its terminal being the
// input, and has one in the narrow layout; a person who may not act sees
// none in either.
func TestDashboardInputBoxWhereThereIsOne(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen ready-for-keys")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	for _, may := range []bool{true, false} {
		if !may {
			dashboardHook = func(c *control.Caller) { c.Principal, c.May = "grace", control.OpRead }
			t.Cleanup(func() { dashboardHook = nil })
		}
		b := newBrowser(t)
		b.call("Page.enable", map[string]any{})
		b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1400, "height": 900, "deviceScaleFactor": 1, "mobile": false})
		b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
		openThreadTerminal(t, b, x.ID, "ready-for-keys")
		b.eval(`document.querySelector('.pane.active .view-switch [data-view=conversation]').click(), true`, nil)
		time.Sleep(500 * time.Millisecond)
		if n := countOf(b, ".input-box"); n != 0 {
			t.Fatalf("may act %v: %d boxes in the wide layout of a terminal thread", may, n)
		}
		b.phone()
		b.until("the narrow layout", `document.querySelector('.shell')?.dataset.layout === 'narrow'`)
		b.eval(fmt.Sprintf(`(() => { const r = document.querySelector('.narrow-list .row[data-thread="%d"]'); if (r) r.click(); return true; })()`, x.ID), nil)
		b.until("the thread in the narrow layout", fmt.Sprintf(`!!document.querySelector('[data-narrow-thread="%d"] .thread-pane')`, x.ID))
		time.Sleep(500 * time.Millisecond)
		if n := countOf(b, ".input-box"); (n == 1) != may {
			t.Fatalf("may act %v: %d boxes in the narrow layout", may, n)
		}
	}
}

// On the web listener the first send asks for the passkey check, then
// sends.
func TestDashboardInputBoxOnTheWeb(t *testing.T) {
	d := newPasskeyDash(t)
	startDaemon(t, d.root)
	startThread(t, d.root, "t1", "reply first")
	c, err := wire.Dial(d.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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
	b.call("WebAuthn.enable", map[string]any{})
	b.call("WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{
		"protocol": "ctap2", "transport": "internal", "hasResidentKey": true, "hasUserVerification": true,
		"isUserVerified": true, "automaticPresenceSimulation": true}})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, d.root, "--web")})
	b.until("the offer to add a passkey", `!!document.querySelector('[data-passkey=offer] [data-passkey=register]')`)
	b.press("[data-passkey=register]")
	b.until("the passkey added", `document.querySelector('[data-passkey=offer]')?.textContent.includes('Passkey added')`)
	openThread(t, b, x.ID)
	b.until("the box", `!!document.querySelector('.pane.active .input-box')`)
	typeInBox(b, "reply from the web")
	b.key("Enter", "Enter", 13, 0, "\r")
	eventually(t, "the send", 20*time.Second, func() bool {
		for _, in := range inputsOf(t, c, x.ID) {
			if in["text"] == "reply from the web" && in["via"] == "dashboard" {
				return true
			}
		}
		return false
	})
	if keys := d.stored(); len(keys) != 1 || keys[0].Used == nil {
		t.Fatalf("no passkey check was made: %+v", keys)
	}
}
