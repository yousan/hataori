package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/wire"
)

// grouped writes n with commas, as the page's number format does in English.
func grouped(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// The "All threads" table, driven by the real server: each column shows
// the control API's value and a dash where the API leaves it out, for a
// thread with every field and one with fields missing; a row opens or
// selects its thread's tab; a state change in the event stream shows in
// its row within two seconds.
func TestDashboardAllThreadsTable(t *testing.T) {
	root := terminalRoot(t)
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
	info := func(id control.ThreadID) control.ThreadInfo {
		t.Helper()
		d, err := c.Thread(ctx, id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return d.ThreadInfo
	}
	// full has every field: a model, usage, a last line, a workspace with
	// changes and an unpushed commit.
	full := start("t1", "usage 1200 300 40000 5000 45000 200000 reported-model\nreply all here", "asked-model")
	// sparse has none it can lack: no model asked, no usage, no text (its
	// agent exits before it says anything, and the session sleeps), and a
	// workspace git cannot read.
	sparse := start("t2", "exit", "")
	eventually(t, "the threads to settle", 20*time.Second, func() bool {
		return info(full).State == control.Idle && info(sparse).State == control.Asleep
	})
	// The workspaces: the session hosts' directories, told apart by the
	// thread each was started for.
	wsOf := map[string]string{}
	dirs, _ := filepath.Glob(filepath.Join(root, "state", "sessions", "*"))
	for _, d := range dirs {
		spec, ok, err := host.ReadSpec(d)
		if err != nil || !ok {
			t.Fatalf("spec of %s: %v", d, err)
		}
		wsOf[d] = spec.Dir
	}
	if len(wsOf) != 2 {
		t.Fatalf("%d sessions", len(wsOf))
	}
	// Tell the two workspaces apart by what the control API says of each
	// after changing one.
	var fullWS, sparseWS string
	for _, w := range wsOf {
		os.WriteFile(filepath.Join(w, "probe"), []byte("x\n"), 0o600)
		st, err := c.Workspace(ctx, full)
		os.Remove(filepath.Join(w, "probe"))
		if err == nil && st.Uncommitted == 1 {
			fullWS = w
		} else {
			sparseWS = w
		}
	}
	if fullWS == "" || sparseWS == "" {
		t.Fatalf("workspaces not told apart: %v", wsOf)
	}
	os.WriteFile(filepath.Join(fullWS, "README"), []byte("changed\nmore\n"), 0o600)
	gitIn(t, fullWS, "commit", "-q", "-am", "local work")
	// Each count differs from the others, so a column showing another's
	// value is seen: +2 −1, 3 uncommitted, 1 unpushed.
	for _, f := range []string{"notes.txt", "todo.txt", "draft.txt"} {
		os.WriteFile(filepath.Join(fullWS, f), []byte("new\n"), 0o600)
	}
	os.WriteFile(filepath.Join(sparseWS, ".git", "HEAD"), []byte("not a ref\n"), 0o600)
	if _, err := c.Workspace(ctx, sparse); err == nil {
		t.Fatal("the sparse thread's workspace can still be read: the check would test nothing")
	}

	base, cookie := dashSignIn(t, root)
	// server is what the page's caller can read of each thread: its info
	// and its workspace, or nil when that cannot be read.
	type server struct {
		x  control.ThreadInfo
		ws *control.WorkspaceState
	}
	read := func(id control.ThreadID) server {
		t.Helper()
		code, body := dashGet(t, base, cookie, "/api/threads")
		var ts []control.ThreadInfo
		if err := json.Unmarshal([]byte(body), &ts); code != http.StatusOK || err != nil {
			t.Fatalf("/api/threads: %d %s", code, body)
		}
		var s server
		for _, x := range ts {
			if x.ID == id {
				s.x = x
			}
		}
		code, body = dashGet(t, base, cookie, "/api/workspace?"+url.Values{"scope": {id.Scope}, "key": {id.Key}}.Encode())
		if code == http.StatusOK {
			s.ws = &control.WorkspaceState{}
			json.Unmarshal([]byte(body), s.ws)
		}
		return s
	}
	const dash = "—"
	// cells is what each column must show for s.
	cells := func(s server) map[string]string {
		x := s.x
		or := func(v string) string {
			if v == "" {
				return dash
			}
			return v
		}
		c := map[string]string{
			"number": "#" + strconv.FormatInt(x.Number, 10),
			"title":  x.Title,
			"agent":  or(x.Agent) + " · " + or(x.Model),
			"last":   or(x.Last),
		}
		c["context"], c["tokens"] = dash, dash
		if x.Context != nil {
			c["context"] = strconv.Itoa(int(float64(100*x.Context.Used)/float64(x.Context.Size)+0.5)) + "%"
		}
		if x.Tokens != nil {
			c["tokens"] = grouped(x.Tokens.Today)
		}
		c["branch"], c["changes"], c["unpushed"] = dash, dash, dash
		if s.ws != nil {
			c["branch"] = or(s.ws.Branch)
			c["changes"] = fmt.Sprintf("+%d −%d · %d files", s.ws.Added, s.ws.Removed, s.ws.Uncommitted)
			c["unpushed"] = grouped(int64(s.ws.Unpushed))
		}
		return c
	}

	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
		document.addEventListener('securitypolicyviolation', e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));`})
	link, err := c.SignInLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.call("Page.navigate", map[string]any{"url": link})
	b.showAll() // one thread is asleep
	b.until("the table with both threads", `document.querySelectorAll('.all-threads tr[data-thread]').length === 2`)
	nFull, nSparse := info(full).Number, info(sparse).Number
	shown := func(n int64) map[string]string {
		var m map[string]string
		b.eval(fmt.Sprintf(`Object.fromEntries([...document.querySelectorAll('.all-threads tr[data-thread="%d"] td')].map(td => [td.dataset.col, td.textContent]))`, n), &m)
		return m
	}

	// 1. Each column: the API's value, or a dash where it is left out.
	for _, th := range []struct {
		id   control.ThreadID
		n    int64
		what string
	}{{full, nFull, "every field"}, {sparse, nSparse, "fields left out"}} {
		var got, want map[string]string
		var s server
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			s = read(th.id)
			got, want = shown(th.n), cells(s)
			ok := true
			for k, v := range want {
				if got[k] != v {
					ok = false
				}
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the row of the thread with %s shows\n%q\nthe API has\n%q\n(%+v, workspace %+v)", th.what, got, want, s.x, s.ws)
			}
		}
		if !strings.HasPrefix(got["age"], string(s.x.State)+" · ") {
			t.Fatalf("state and age: %q, state %s", got["age"], s.x.State)
		}
		var since string
		b.eval(fmt.Sprintf(`document.querySelector('.all-threads tr[data-thread="%d"] td[data-col=age]').dataset.since`, th.n), &since)
		if at, err := time.Parse(time.RFC3339Nano, since); err != nil || !at.Equal(s.x.Updated) {
			t.Fatalf("the age counts from %q, the state changed at %v", since, s.x.Updated)
		}
	}
	s := read(full)
	if s.x.Model == "" || s.x.Context == nil || s.x.Tokens == nil || s.x.Last == "" || s.ws == nil || s.ws.Unpushed != 1 || s.ws.Uncommitted != 3 || s.ws.Added != 2 || s.ws.Removed != 1 {
		t.Fatalf("the full thread lacks a field: the check would test nothing: %+v %+v", s.x, s.ws)
	}
	if s = read(sparse); s.x.Model != "" || s.x.Context != nil || s.x.Tokens != nil || s.x.Last != "" || s.ws != nil {
		t.Fatalf("the sparse thread has a field: the check would test nothing: %+v %+v", s.x, s.ws)
	}

	// 2. A row opens the thread's tab, or selects it when it is open.
	activeTab := `document.querySelector('.tabbar .tab-main[aria-selected=true]')?.closest('.tab').dataset.tab`
	openTabs := `[...document.querySelectorAll('.tabbar .tab')].map(x => x.dataset.tab).join(',')`
	clickRow := func(n int64) {
		t.Helper()
		b.click(`.tab[data-tab=all] .tab-main`)
		b.until("the table", `!!document.querySelector('.all-threads')`)
		b.click(fmt.Sprintf(`.all-threads tr[data-thread="%d"]`, n))
	}
	clickRow(nSparse)
	b.until("the sparse thread's tab, opened and active", fmt.Sprintf(`%s === 'thread:%d' && %s === 'all,thread:%d'`, activeTab, nSparse, openTabs, nSparse))
	clickRow(nFull)
	b.until("the full thread's tab, opened and active", fmt.Sprintf(`%s === 'thread:%d' && %s === 'all,thread:%d,thread:%d'`, activeTab, nFull, openTabs, nSparse, nFull))
	clickRow(nSparse)
	b.until("the sparse thread's tab selected, not opened again", fmt.Sprintf(`%s === 'thread:%d' && %s === 'all,thread:%d,thread:%d'`, activeTab, nSparse, openTabs, nSparse, nFull))
	// From the keyboard: the row's open button, focused, takes Enter.
	b.click(`.tab[data-tab=all] .tab-main`)
	b.until("the table", `!!document.querySelector('.all-threads')`)
	b.eval(fmt.Sprintf(`document.querySelector('.all-threads tr[data-thread="%d"] .row-open').focus(), true`, nFull), nil)
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "text": "\r"})
	b.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13})
	b.until("the full thread's tab selected from the keyboard", fmt.Sprintf(`%s === 'thread:%d' && %s === 'all,thread:%d,thread:%d'`, activeTab, nFull, openTabs, nSparse, nFull))
	b.click(`.tab[data-tab=all] .tab-main`)
	b.until("the table", `!!document.querySelector('.all-threads')`)

	// 3. A state change in the event stream shows in the row within two
	// seconds of the event.
	changed := func(id control.ThreadID, n int64, state control.State, after time.Time) {
		t.Helper()
		var seen time.Time
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var st string
			b.eval(fmt.Sprintf(`document.querySelector('.all-threads tr[data-thread="%d"]')?.dataset.state ?? ''`, n), &st)
			if st == string(state) {
				seen = time.Now()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the row of %d never showed %s", n, state)
			}
		}
		_, out, _ := cli(t, "events", "--root", root)
		var at time.Time
		for _, e := range lines(t, out) {
			if th, _ := e["thread"].(map[string]any); e["kind"] != "state" || th["scope"] != id.Scope || th["key"] != id.Key {
				continue
			}
			d, _ := e["data"].(map[string]any)
			if d["to"] == string(state) {
				if tm, err := time.Parse(time.RFC3339Nano, e["time"].(string)); err == nil && tm.After(after) {
					at = tm
				}
			}
		}
		if at.IsZero() {
			t.Fatalf("no state event to %s after %v", state, after)
		}
		if lag := seen.Sub(at); lag > 2*time.Second {
			t.Fatalf("the row showed %s %v after the event, more than 2s", state, lag)
		}
		var cell string
		b.eval(fmt.Sprintf(`document.querySelector('.all-threads tr[data-thread="%d"] td[data-col=age]').textContent`, n), &cell)
		if !strings.HasPrefix(cell, string(state)+" · ") {
			t.Fatalf("the row's state cell shows %q for %s", cell, state)
		}
	}
	before := time.Now().Add(-time.Second)
	if err := c.Send(ctx, full, "k-ask", control.Input{Text: "ask Go on?"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	changed(full, nFull, control.Waiting, before)
	d, err := c.Thread(ctx, full, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Requests) != 1 {
		t.Fatalf("the question: %+v", d.Requests)
	}
	before = time.Now().Add(-time.Second)
	if err := c.Answer(ctx, control.RequestID(d.Requests[0].ID), control.Answer{Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	changed(full, nFull, control.Idle, before)

	var v []string
	b.eval(`window.__csp`, &v)
	if len(v) > 0 {
		t.Fatalf("Content-Security-Policy violations: %q", v)
	}
}
