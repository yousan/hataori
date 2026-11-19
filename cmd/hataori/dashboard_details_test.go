package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/wire"
)

// The details panel, driven by the real server: a question's choices are
// text with no control that answers; every field is the control API's
// value or a dash where the API leaves it out, for a thread with every
// field, one with fields left out, a question and a failure; and the
// recent events follow the event stream.
func TestDashboardDetailsPanel(t *testing.T) {
	root := terminalRoot(t)
	// The fake agent's first start exits at once while its session is
	// being created: that thread fails. Later starts run the fake agent.
	once := filepath.Join(t.TempDir(), "fail-once")
	os.WriteFile(once, nil, 0o600)
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
	start := func(key, text, model string) control.ThreadID {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: "k-" + key, Scope: "s", Thread: key, Repo: repo, Input: control.Input{Text: text}, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	info := func(id control.ThreadID) control.ThreadDetail {
		t.Helper()
		d, err := c.Thread(ctx, id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	failed := start("t1", "reply never", "")
	eventually(t, "the first thread to fail", 20*time.Second, func() bool { return info(failed).State == control.Failed })
	asking := start("t2", "ask Which colour? -- red | green | blue", "")
	full := start("t3", "usage 1200 300 40000 5000 45000 200000 reported-model\nreply all here", "asked-model")
	sparse := start("t4", "exit", "")
	eventually(t, "the threads to settle", 20*time.Second, func() bool {
		return info(asking).State == control.Waiting && info(full).State == control.Idle && info(sparse).State == control.Asleep
	})
	// The sparse thread's workspace cannot be read; the full one has
	// changes. Each session's directory names its workspace.
	for _, d := range func() []string { ds, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*")); return ds }() {
		spec, ok, _ := host.ReadSpec(d)
		if !ok {
			continue
		}
		os.WriteFile(filepath.Join(spec.Dir, "probe"), nil, 0o600)
		fs, err1 := c.Workspace(ctx, full)
		ss, err2 := c.Workspace(ctx, sparse)
		os.Remove(filepath.Join(spec.Dir, "probe"))
		switch {
		case err1 == nil && fs.Uncommitted == 1:
			os.WriteFile(filepath.Join(spec.Dir, "README"), []byte("changed\nmore\n"), 0o600)
			gitIn(t, spec.Dir, "commit", "-q", "-am", "local work")
			for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
				os.WriteFile(filepath.Join(spec.Dir, f), nil, 0o600)
			}
		case err2 == nil && ss.Uncommitted == 1:
			os.WriteFile(filepath.Join(spec.Dir, ".git", "HEAD"), []byte("not a ref\n"), 0o600)
		}
	}
	if _, err := c.Workspace(ctx, sparse); err == nil {
		t.Fatal("the sparse thread's workspace can still be read: the check would test nothing")
	}
	if ws, err := c.Workspace(ctx, full); err != nil || ws.Unpushed != 1 || ws.Uncommitted != 3 {
		t.Fatalf("the full thread's workspace: %+v %v", ws, err)
	}

	base, cookie := dashSignIn(t, root)
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
	b.showAll() // one thread is asleep
	b.until("the four threads", `document.querySelectorAll('[data-section=threads] .row').length === 4`)
	open := func(n int64) {
		t.Helper()
		b.click(fmt.Sprintf(`[data-section=threads] .row[data-thread="%d"]`, n))
		b.until(fmt.Sprintf("the details of %d", n), fmt.Sprintf(`!!document.querySelector('.details [data-details="%d"]')`, n))
	}
	field := func(name string) string {
		var s string
		b.eval(fmt.Sprintf(`document.querySelector('.details [data-field=%q] dd')?.textContent ?? document.querySelector('.details [data-field=%q]')?.textContent ?? null`, name, name), &s)
		return s
	}

	// 1. The question and its choices, as text only.
	nQ := int64(info(asking).ID)
	open(nQ)
	b.until("the question and its choices", `document.querySelector('.details [data-field=question]')?.textContent === 'Which colour?'`+
		` && JSON.stringify([...document.querySelectorAll('.details [data-field=choices] li')].map(li => li.textContent)) === '["red","green","blue"]'`)
	controls := `document.querySelectorAll('.details button, .details a[href], .details input, .details select, .details textarea, .details [role=button], .details [role=option], .details [role=checkbox], .details [role=radio], .details [tabindex], .details [contenteditable], .details summary').length`
	var n int
	b.eval(controls, &n)
	if n != 0 {
		var html string
		b.eval(`document.querySelector('.details').innerHTML`, &html)
		t.Fatalf("the details panel has %d controls:\n%s", n, html)
	}
	var clickable bool
	b.eval(`[...document.querySelectorAll('.details [data-field=choices] li')].some(li => getComputedStyle(li).cursor === 'pointer')`, &clickable)
	if clickable {
		t.Fatal("a choice looks like something to click")
	}
	requestsBefore := len(b.events)
	b.eval(`[...document.querySelectorAll('.details [data-field=choices] li')].forEach(li => li.click()), true`, nil)
	time.Sleep(500 * time.Millisecond)
	b.eval(`1`, nil)
	for _, e := range b.events[requestsBefore:] {
		if e.Method != "Network.requestWillBeSent" {
			continue
		}
		var p struct{ Request struct{ URL, Method string } }
		json.Unmarshal(e.Params, &p)
		if p.Request.Method != "GET" && p.Request.Method != "HEAD" {
			t.Fatalf("clicking a choice sent %s %s", p.Request.Method, p.Request.URL)
		}
	}
	if d := info(asking); d.State != control.Waiting || len(d.Requests) != 1 {
		t.Fatalf("after clicking the choices the question is %s, %+v", d.State, d.Requests)
	}

	// 2. Every field: the API's value, or a dash where it is left out; for
	// the question, the failure, and threads with every field and without.
	const dash = "—"
	or := func(s string) string {
		if s == "" {
			return dash
		}
		return s
	}
	num := func(v *int64) string {
		if v == nil {
			return dash
		}
		return grouped(*v)
	}
	expect := func(x control.ThreadInfo) map[string]string {
		t.Helper()
		m := map[string]string{
			"state": string(x.State), "scope": x.Scope, "startedBy": or(string(x.StartedBy)),
			"agent": or(x.Agent), "model": or(x.Model), "effort": or(x.Effort), "repo": or(x.Repo), "terminal": or(x.Terminal),
			"turns": grouped(int64(x.Turns)), "background": dash, "context": dash, "tokensTurn": dash, "tokensToday": dash,
			"branch": dash, "lines": dash, "uncommitted": dash, "unpushed": dash,
		}
		if x.Background != 0 {
			m["background"] = grouped(int64(x.Background))
		}
		if x.Context != nil && x.Context.Size > 0 {
			m["context"] = fmt.Sprintf("%d%% (%s of %s)", int(float64(100*x.Context.Used)/float64(x.Context.Size)+0.5), grouped(x.Context.Used), grouped(x.Context.Size))
		}
		if x.Tokens != nil {
			m["tokensTurn"], m["tokensToday"] = num(&x.Tokens.Turn), num(&x.Tokens.Today)
		}
		code, body := dashGet(t, base, cookie, "/api/workspace?"+url.Values{"thread": {fmt.Sprint(int64(x.ID))}}.Encode())
		if code == http.StatusOK {
			var ws control.WorkspaceState
			json.Unmarshal([]byte(body), &ws)
			m["branch"] = or(ws.Branch)
			m["lines"] = fmt.Sprintf("+%d −%d", ws.Added, ws.Removed)
			m["uncommitted"], m["unpushed"] = grouped(int64(ws.Uncommitted)), grouped(int64(ws.Unpushed))
		}
		return m
	}
	// apiThread is the thread as the page's caller reads it.
	apiThread := func(id control.ThreadID) control.ThreadInfo {
		t.Helper()
		_, body := dashGet(t, base, cookie, "/api/threads")
		var ts []control.ThreadInfo
		json.Unmarshal([]byte(body), &ts)
		for _, x := range ts {
			if x.ID == id {
				return x
			}
		}
		t.Fatalf("thread %v not listed", id)
		return control.ThreadInfo{}
	}
	matches := func(id control.ThreadID, what string) {
		t.Helper()
		var got, want map[string]string
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			x := apiThread(id)
			want = expect(x)
			got = map[string]string{}
			for k := range want {
				got[k] = field(k)
			}
			var created string
			b.eval(`document.querySelector('.details [data-field=created] time')?.getAttribute('datetime') ?? ''`, &created)
			at, err := time.Parse(time.RFC3339Nano, created)
			if reflect.DeepEqual(got, want) && err == nil && at.Equal(x.Created) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the panel shows\n%q\nthe API has\n%q\n(created %q, want %v)", what, got, want, created, x.Created)
			}
		}
	}
	matches(asking, "the question")
	nF := int64(info(failed).ID)
	open(nF)
	matches(failed, "the failure")
	var reason string
	_, out, _ := cli(t, "events", "--root", root)
	for _, e := range lines(t, out) {
		if e["kind"] == "state" && e["thread"] == float64(failed) && e["data"].(map[string]any)["to"] == "failed" {
			reason, _ = e["data"].(map[string]any)["reason"].(string)
		}
	}
	if reason == "" {
		t.Fatal("the failure has no reason in its state event: the check would test nothing")
	}
	b.until("the failure's reason", fmt.Sprintf(`document.querySelector('.details [data-block=failure] [data-field=reason]')?.textContent === %q`, reason))
	nFull, nSparse := int64(info(full).ID), int64(info(sparse).ID)
	open(nFull)
	matches(full, "the thread with every field")
	if x := apiThread(full); x.Model == "" || x.Context == nil || x.Tokens == nil || x.Turns == 0 || x.Terminal == "" {
		t.Fatalf("the full thread lacks a field: the check would test nothing: %+v", x)
	}
	if strings.Contains(field("branch"), dash) || field("unpushed") != "1" || field("uncommitted") != "3" || field("lines") != "+2 −1" {
		t.Fatalf("the full thread's workspace in the panel: branch %q, lines %q, uncommitted %q, unpushed %q", field("branch"), field("lines"), field("uncommitted"), field("unpushed"))
	}
	open(nSparse)
	matches(sparse, "the thread with fields left out")
	if x := apiThread(sparse); x.Model != "" || x.Context != nil || x.Tokens != nil || x.Terminal != "" {
		t.Fatalf("the sparse thread has a field: the check would test nothing: %+v", x)
	}
	for _, f := range []string{"model", "context", "tokensToday", "branch", "lines", "uncommitted", "unpushed", "terminal"} {
		if field(f) != dash {
			t.Fatalf("the sparse thread's %s shows %q, want a dash", f, field(f))
		}
	}

	// A workspace changed by hand shows when its thread is selected again,
	// with no table on the screen.
	fullDir := ""
	for _, d := range func() []string { ds, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*")); return ds }() {
		if spec, ok, _ := host.ReadSpec(d); ok {
			if _, err := os.Stat(filepath.Join(spec.Dir, "a.txt")); err == nil {
				fullDir = spec.Dir
			}
		}
	}
	os.WriteFile(filepath.Join(fullDir, "d.txt"), nil, 0o600)
	open(nFull)
	b.until("the hand-made change in the panel", `document.querySelector('.details [data-field=uncommitted] dd')?.textContent === '4'`)

	// 3. Recent events follow the stream: the panel lists the thread's last
	// events, newest first, also those of a turn after it opened.
	open(nFull)
	recent := func() bool {
		t.Helper()
		var shown []string
		b.eval(`[...document.querySelectorAll('.details [data-block=events] li')].map(li => li.dataset.pos + ' ' + li.dataset.kind)`, &shown)
		_, out, _ := cli(t, "events", "--root", root)
		var want []string
		for _, e := range lines(t, out) {
			if e["thread"] == float64(full) {
				want = append([]string{strconv.FormatInt(int64(e["pos"].(float64)), 10) + " " + e["kind"].(string)}, want...)
			}
		}
		if len(want) > 20 {
			want = want[:20]
		}
		return reflect.DeepEqual(shown, want)
	}
	eventually(t, "the recent events", 20*time.Second, recent)
	if err := c.Send(ctx, full, "k-again", control.Input{Text: "tool quick 10ms\nreply again"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second turn to end", 20*time.Second, func() bool { d := info(full); return d.State == control.Idle && d.Turns == 2 })
	eventually(t, "the recent events after a new turn", 20*time.Second, recent)
	var last string
	b.eval(`document.querySelector('.details [data-block=events] li .recent-line')?.textContent ?? ''`, &last)
	if !strings.HasPrefix(last, "state: ") && !strings.HasPrefix(last, "turn ended") {
		t.Fatalf("the newest recent event reads %q", last)
	}

	var v []string
	b.eval(`window.__csp`, &v)
	if len(v) > 0 {
		t.Fatalf("Content-Security-Policy violations: %q", v)
	}
}
