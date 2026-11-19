package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// browser is a headless Chrome driven over the DevTools protocol: the
// dashboard's page as a person's browser runs it, against a real daemon.
type browser struct {
	t       browserTest
	conn    *websocket.Conn
	id      int
	timeout time.Duration
	// events are the DevTools events read so far, in order, such as the
	// page's requests once Network.enable is called.
	events []cdpEvent
}

// browserTest is the part of testing.T used by the browser.
type browserTest interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// cdpEvent is one DevTools event.
type cdpEvent struct {
	Method string
	Params json.RawMessage
}

// chromePath finds Chrome or Chromium. Without one the test is skipped,
// except in CI, where it fails.
func chromePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("HATAORI_TEST_CHROME"); p != "" {
		return p
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatal("no Chrome or Chromium to run the dashboard's page in; set HATAORI_TEST_CHROME")
	}
	t.Skip("no Chrome or Chromium; set HATAORI_TEST_CHROME to run the dashboard's page")
	return ""
}

// newBrowser starts a headless browser on an empty page, with flags of
// the test's own added.
func newBrowser(t *testing.T, flags ...string) *browser {
	t.Helper()
	// The profile is removed once every process of the browser is gone:
	// its helpers write there until they end.
	dir, err := os.MkdirTemp("", "hataori-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(chromePath(t), "--headless=new", "--no-sandbox", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-background-networking", "--disable-extensions",
		// A throwaway profile needs no keychain; without these, macOS asks for one on the desktop
		// each run when the test runs outside the graphical session.
		"--use-mock-keychain", "--password-store=basic",
		"--window-size=1400,900", "--user-data-dir="+dir, "--remote-debugging-port=0")
	cmd.Args = append(append(cmd.Args, flags...), "about:blank")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // the browser and its helpers, in one group
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pgid := cmd.Process.Pid
		syscall.Kill(-pgid, syscall.SIGKILL)
		cmd.Wait()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
				break
			}
		}
		for i := 0; i < 50; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("the browser's profile %s was not removed", dir)
	})
	var port string
	eventually(t, "the browser to listen", 30*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
		if err != nil {
			return false
		}
		port, _, _ = strings.Cut(string(b), "\n")
		return port != ""
	})
	var targets []struct{ Type, WebSocketDebuggerURL string }
	eventually(t, "the browser's page", 10*time.Second, func() bool {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(&targets)
		return slices.ContainsFunc(targets, func(x struct{ Type, WebSocketDebuggerURL string }) bool { return x.Type == "page" })
	})
	i := slices.IndexFunc(targets, func(x struct{ Type, WebSocketDebuggerURL string }) bool { return x.Type == "page" })
	conn, _, err := websocket.DefaultDialer.Dial(targets[i].WebSocketDebuggerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &browser{t: t, conn: conn, timeout: 20 * time.Second}
}

// call sends one DevTools command and returns its result.
func (b *browser) call(method string, params any) json.RawMessage {
	b.t.Helper()
	b.id++
	b.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	b.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := b.conn.WriteJSON(map[string]any{"id": b.id, "method": method, "params": params}); err != nil {
		b.t.Fatal(err)
	}
	for {
		var m struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct{ Message string }
		}
		if err := b.conn.ReadJSON(&m); err != nil {
			b.t.Fatalf("%s: %v", method, err)
		}
		if m.ID == 0 && m.Method != "" {
			b.events = append(b.events, cdpEvent{m.Method, m.Params})
			continue
		}
		if m.ID != b.id {
			continue
		}
		if m.Error != nil {
			b.t.Fatalf("%s: %s", method, m.Error.Message)
		}
		return m.Result
	}
}

// eval runs expr in the page and decodes its value into v.
func (b *browser) eval(expr string, v any) {
	b.t.Helper()
	if exception := b.evalException(expr, v); exception != "" {
		b.t.Fatalf("%s: %s", expr, exception)
	}
}

// evalException runs expr and returns the exception, if it threw.
func (b *browser) evalException(expr string, v any) string {
	b.t.Helper()
	var r struct {
		Result           struct{ Value json.RawMessage }
		ExceptionDetails *struct {
			Text      string
			Exception *struct{ Description string }
		}
	}
	json.Unmarshal(b.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}), &r)
	if e := r.ExceptionDetails; e != nil {
		text := e.Text
		if e.Exception != nil && e.Exception.Description != "" {
			text += ": " + e.Exception.Description
		}
		if text == "" {
			text = "JavaScript exception"
		}
		return text
	}
	if v != nil {
		if err := json.Unmarshal(r.Result.Value, v); err != nil {
			b.t.Fatalf("%s: %s: %v", expr, r.Result.Value, err)
		}
	}
	return ""
}

// until waits for expr, a JavaScript expression, to be true in the page.
func (b *browser) until(what, expr string) {
	b.t.Helper()
	var lastException string
	for deadline := time.Now().Add(b.timeout); ; time.Sleep(50 * time.Millisecond) {
		var ok bool
		if exception := b.evalException("!!("+expr+")", &ok); exception != "" {
			lastException = exception
		} else if ok {
			return
		}
		if time.Now().After(deadline) {
			var html string
			if exception := b.evalException("document.body.innerHTML", &html); exception != "" {
				html = "reading the page's HTML threw: " + exception
			}
			if lastException != "" {
				b.t.Fatalf("the page never showed %s\nlast exception: %s\n%s", what, lastException, html)
			}
			b.t.Fatalf("the page never showed %s\n%s", what, html)
		}
	}
}

// rows is the thread numbers the rows of a section show, in order.
func (b *browser) rows(section string) []int64 {
	b.t.Helper()
	var ns []int64
	b.eval(fmt.Sprintf(`[...document.querySelectorAll('[data-section=%q] .row')].map(r => +r.dataset.thread)`, section), &ns)
	return ns
}

// filter types q into the filter box, as a person would.
func (b *browser) filter(q string) {
	b.t.Helper()
	b.eval(fmt.Sprintf(`(() => {
		const el = document.querySelector('.filter');
		Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(el, %q);
		el.dispatchEvent(new Event('input', {bubbles: true}));
		return true;
	})()`, q), nil)
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

// The tree and the status bar, driven by the real server: a question
// enters needs attention and leaves it when answered; the footer counts
// the threads per state; the filter matches title, number and terminal;
// the normal density hides every row's second line; the live mark shows
// the last event's position and turns off when the stream goes down.
func TestDashboardTreeAndStatusBar(t *testing.T) {
	root := terminalRoot(t) // the fake agent in terminals; the page's caller sees scope s
	stop := runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	repo := bareRepo(t)
	start := func(key, text string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: "k-" + key, Scope: "s", Thread: key, Repo: repo, Input: control.Input{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	asking := start("t1", "ask Proceed with the deploy?")
	start("t2", "reply done")
	start("t3", "tool long 10m")
	if _, err := c.Start(ctx, control.StartRequest{Key: "k-other", Scope: "x", Thread: "t4", Repo: repo, Input: control.Input{Text: "reply elsewhere"}}); err != nil {
		t.Fatal(err)
	}

	base, cookie := dashSignIn(t, root)
	// threads is the page's caller's threads, from the server, by title.
	threads := func() map[string]control.ThreadInfo {
		t.Helper()
		code, body := dashGet(t, base, cookie, "/api/threads")
		var ts []control.ThreadInfo
		if err := json.Unmarshal([]byte(body), &ts); code != http.StatusOK || err != nil {
			t.Fatalf("/api/threads: %d %s", code, body)
		}
		m := map[string]control.ThreadInfo{}
		for _, x := range ts {
			m[x.Title] = x
		}
		return m
	}
	var ts map[string]control.ThreadInfo
	eventually(t, "the threads to settle", 20*time.Second, func() bool {
		ts = threads()
		return len(ts) == 3 && ts["ask Proceed with the deploy?"].State == control.Waiting &&
			ts["reply done"].State == control.Idle && ts["tool long 10m"].State == control.Running
	})
	n1, n2, n3 := int64(ts["ask Proceed with the deploy?"].ID), int64(ts["reply done"].ID), int64(ts["tool long 10m"].ID)

	b := newBrowser(t)
	link, err := c.SignInLink(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the three threads", `document.querySelectorAll('[data-section=threads] .row').length === 3`)

	// 1. The question, in needs attention; answered, it leaves.
	// The question has a line of its own, apart from the title, and keeps it
	// in the normal density.
	question := fmt.Sprintf(`(() => {
		const w = document.querySelector('[data-section=attention] .row[data-thread="%d"] .why');
		return !!w && w.textContent === 'Question: Proceed with the deploy?' && getComputedStyle(w).display !== 'none';
	})()`, n1)
	b.until("the question in needs attention", question)
	if got := b.rows("attention"); !reflect.DeepEqual(got, []int64{n1}) {
		t.Fatalf("needs attention: threads %v, want [%d]", got, n1)
	}
	b.eval(`document.querySelector('[data-density=normal]').click(), true`, nil)
	b.until("the question in the normal density", question)
	b.eval(`document.querySelector('[data-density=dense]').click(), true`, nil)
	d, err := c.Thread(ctx, asking, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("the question: %+v, %v", d.Requests, err)
	}
	if err := c.Answer(ctx, control.RequestID(d.Requests[0].ID), control.Answer{Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	b.until("the answered thread to leave needs attention", `!document.querySelector('[data-section=attention]')`)

	// 2. The footer counts the threads in each state, as the server has them.
	countsMatch := func() bool {
		want := map[string]int{}
		for _, x := range threads() {
			want[string(x.State)]++
		}
		var got map[string]int
		b.eval(`Object.fromEntries([...document.querySelectorAll('.counts [data-count]')].map(li => [li.dataset.count, +li.dataset.n]))`, &got)
		var text string
		b.eval(`document.querySelector('.counts').textContent`, &text)
		for s, n := range want {
			if !strings.Contains(text, s+" "+strconv.Itoa(n)) {
				return false
			}
		}
		return reflect.DeepEqual(got, want)
	}
	eventually(t, "the footer to count the threads per state", 20*time.Second, countsMatch)

	// 3. The filter: title, number, terminal; empty shows everything.
	ts = threads()
	// The terminal case uses the longest terminal: a shorter one can sit
	// inside the others (/dev/pts/1 in /dev/pts/10 and /dev/pts/11).
	var term string
	var termHit int64
	for _, x := range ts {
		if len(x.Terminal) > len(term) {
			term, termHit = x.Terminal, int64(x.ID)
		}
	}
	term2 := ts["reply done"].Terminal
	if term2 == "" {
		t.Fatalf("thread %d has no terminal: %+v", n2, ts["reply done"])
	}
	// expect is the threads, in the page's order, that the filter q
	// should show: the title holds q, the number is q (with or without #),
	// or the terminal holds q.
	expect := func(q string) []int64 {
		var out []int64
		for _, x := range ts {
			f := strings.ToLower(q)
			if strings.Contains(strings.ToLower(x.Title), f) || strconv.FormatInt(int64(x.ID), 10) == strings.TrimPrefix(f, "#") ||
				(x.Terminal != "" && strings.Contains(strings.ToLower(x.Terminal), f)) {
				out = append(out, int64(x.ID))
			}
		}
		slices.Sort(out)
		return out
	}
	type filterCase struct {
		q   string
		hit int64 // a thread the filter must show
	}
	cases := []filterCase{
		{"DEPLOY", n1},
		{"#" + strconv.FormatInt(n2, 10), n2},
		{term, termHit},
	}
	// A bare number also matches every terminal that holds its digits,
	// and the terminals are whatever devices the host hands out (thread 3
	// on /dev/pts/13 beside /dev/pts/23 and /dev/pts/30). So the bare
	// number is a thread's whose title and terminal do not hold it (only
	// its number shows it) and that leaves at least one thread out.
	bare := false
	for _, x := range ts {
		q := strconv.FormatInt(int64(x.ID), 10)
		if strings.Contains(x.Title, q) || strings.Contains(x.Terminal, q) || len(expect(q)) == 3 {
			continue
		}
		cases = append(cases, filterCase{q, int64(x.ID)})
		bare = true
		break
	}
	if !bare {
		t.Logf("no thread's bare number singles it out on these terminals, so it is not checked: %+v", ts)
	}
	for _, f := range cases {
		want := expect(f.q)
		if !slices.Contains(want, f.hit) || len(want) == 3 {
			t.Fatalf("filter %q: the case does not single out thread %d: %v", f.q, f.hit, want)
		}
		b.filter(f.q)
		b.until(fmt.Sprintf("filter %q", f.q), fmt.Sprintf(
			`JSON.stringify([...document.querySelectorAll('[data-section=threads] .row')].map(r => +r.dataset.thread).sort((a, b) => a - b)) === %q`, js(want)))
	}
	b.filter("")
	b.until("every thread with an empty filter", fmt.Sprintf(
		`JSON.stringify([...document.querySelectorAll('[data-section=threads] .row')].map(r => +r.dataset.thread).sort((a, b) => a - b)) === %q`, js([]int64{n1, n2, n3})))

	// 4. Normal density hides the second line of every row; dense shows it.
	hidden := `[...document.querySelectorAll('.row')].every(r => getComputedStyle(r.querySelector('.line2')).display === 'none')`
	shown := `[...document.querySelectorAll('.row')].every(r => getComputedStyle(r.querySelector('.line2')).display !== 'none')`
	b.until("dense rows with their second line", shown)
	b.eval(`document.querySelector('[data-density=normal]').click(), true`, nil)
	b.until("normal rows without a second line", hidden)
	b.eval(`document.querySelector('[data-density=dense]').click(), true`, nil)
	b.until("dense rows again", shown)

	// The status bar shows the selected thread, with its terminal. A row is
	// selected from the keyboard as well.
	b.eval(fmt.Sprintf(`document.querySelector('[data-section=threads] .row[data-thread="%d"]').focus(), true`, n2), nil)
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13})
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13})
	b.until("the selected thread in the status bar", fmt.Sprintf(
		`document.querySelector('.status-item').dataset.selected == %d && document.querySelector('.status-item').textContent.includes(%q)`, n2, term2))

	// 5. The live mark: the position of the last event of the caller's
	// threads, also after a new one; off when the stream is down.
	last := func() uint64 {
		t.Helper()
		_, out, _ := cli(t, "events", "--root", root)
		var pos uint64
		for _, e := range lines(t, out) {
			if e["scope"] == "s" {
				pos = max(pos, uint64(e["pos"].(float64)))
			}
		}
		return pos
	}
	// liveAtLast waits for the mark to be on at the last event, reading
	// both again each time, as events may still come.
	liveAtLast := func(what string) {
		t.Helper()
		var page struct {
			On   bool
			Pos  string
			Text string
		}
		var want uint64
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			want = last()
			b.eval(`(() => { const l = document.querySelector('.live'); return {on: l.dataset.live === 'on', pos: l.dataset.pos, text: l.textContent}; })()`, &page)
			p := strconv.FormatUint(want, 10)
			if page.On && page.Pos == p && strings.Contains(page.Text, p) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the live mark %+v, the last event %d", what, page, want)
			}
		}
	}
	liveAtLast("the live mark at the last event")
	if err := c.Send(ctx, ts["reply done"].ID, "k-again", control.Input{Text: "reply again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new turn to end", 15*time.Second, func() bool {
		return threads()["reply done"].Turns == 2 && threads()["reply done"].State == control.Idle
	})
	liveAtLast("the live mark at the new last event")
	c.Close()
	stop()
	b.until("the live mark off", `document.querySelector('.live').dataset.live === 'off'`)
}
