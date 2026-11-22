package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/wire"
)

// actRig is a daemon with both dashboard listeners, the fake agent and
// one thread, for the routes that give a thread input (#606).
type actRig struct {
	*passkeyDash
	c      *wire.Client
	id     control.ThreadID
	logged func() string
}

func newActRig(t *testing.T) *actRig {
	t.Helper()
	r := &actRig{passkeyDash: newPasskeyDash(t)}
	r.logged = startDaemonLog(t, r.root)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	r.c = c
	if r.id, err = c.Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply hello"}}); err != nil {
		t.Fatal(err)
	}
	r.until(t, "the first turn", func(evs []control.Event) bool { return count(evs, "turn_ended") == 1 })
	return r
}

// events are the thread's events, as the owner reads them.
func (r *actRig) events(t *testing.T) []control.Event {
	t.Helper()
	evs, err := r.c.ThreadEvents(context.Background(), r.id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (r *actRig) until(t *testing.T, what string, ok func([]control.Event) bool) []control.Event {
	t.Helper()
	var evs []control.Event
	eventually(t, what, 20*time.Second, func() bool { evs = r.events(t); return ok(evs) })
	return evs
}

func count(evs []control.Event, kind control.EventKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// dataOf is the data of each event of kind, in order.
func dataOf(evs []control.Event, kind control.EventKind) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e.Kind == kind {
			var m map[string]any
			json.Unmarshal(e.Data, &m)
			out = append(out, m)
		}
	}
	return out
}

// local is the loopback listener's address and a session of it.
func (r *actRig) local(t *testing.T) (string, dashSession) {
	t.Helper()
	link := signinLink(t, r.root)
	u, _ := url.Parse(link)
	s, _ := r.trade(link)
	return u.Scheme + "://" + u.Host, s
}

// act posts body to path on base from the page, with session s.
func (r *actRig) act(base, path string, s dashSession, body any) (int, string) {
	r.t.Helper()
	if base == "" {
		base = r.web
	}
	code, b := r.do(http.MethodPost, base, path, s, base, body)
	return code, string(b)
}

// On the loopback listener the owner sends, answers and stops at once:
// a send reaches the agent once, also when it is sent again with the same
// key; an answer answers the open question, and a second one is refused as
// not open; a stop interrupts the running turn, and a stop of a turn that
// ended stops no later one. Each event says it came through the dashboard
// and names the owner. Unknown threads and requests are 404; a request
// without the page's Origin is refused, and a GET is not served.
func TestDashboardActOnTheLoopbackListener(t *testing.T) {
	r := newActRig(t)
	base, s := r.local(t)
	if code, b := r.do(http.MethodGet, base, dashboard.ActMayPath, s, "", nil); code != http.StatusOK || !strings.Contains(string(b), `"may":true`) {
		t.Fatalf("act/may for the owner: %d %s", code, b)
	}
	if code, b := r.act(base, dashboard.ActCheckPath, s, map[string]any{}); code != http.StatusOK || !strings.Contains(b, `"needs":"nothing"`) {
		t.Fatalf("act/check on loopback: %d %s", code, b)
	}

	// Send, twice with the same key.
	send := map[string]any{"thread": r.id, "key": "p1", "text": "reply from the page"}
	for range 2 {
		if code, b := r.act(base, dashboard.SendPath, s, send); code != http.StatusNoContent {
			t.Fatalf("send: %d %s", code, b)
		}
	}
	evs := r.until(t, "the page's turn", func(evs []control.Event) bool { return count(evs, "turn_ended") == 2 })
	time.Sleep(300 * time.Millisecond)
	evs = r.events(t)
	in := dataOf(evs, "input")
	if len(in) != 2 || in[1]["text"] != "reply from the page" || in[1]["via"] != "dashboard" || in[1]["principal"] != "owner" {
		t.Fatalf("input events %v, want the page's once, via dashboard, by the owner", in)
	}

	// Answer, then again.
	if code, b := r.act(base, dashboard.SendPath, s, map[string]any{"thread": r.id, "key": "p2", "text": "ask which? -- red | blue"}); code != http.StatusNoContent {
		t.Fatalf("send: %d %s", code, b)
	}
	r.until(t, "the question", func(evs []control.Event) bool { return count(evs, "request") == 1 })
	d, err := r.c.Thread(context.Background(), r.id, 0, 0)
	if err != nil || len(d.Requests) != 1 || len(d.Requests[0].Choices) != 2 {
		t.Fatalf("the open question: %+v %v", d.Requests, err)
	}
	q := d.Requests[0]
	answer := map[string]any{"request": q.ID, "choices": []string{q.Choices[1].Token}}
	if code, b := r.act(base, dashboard.AnswerPath, s, answer); code != http.StatusNoContent {
		t.Fatalf("answer: %d %s", code, b)
	}
	evs = r.until(t, "the answer", func(evs []control.Event) bool { return count(evs, "answer") == 1 })
	if a := dataOf(evs, "answer")[0]; a["via"] != "dashboard" || a["principal"] != "owner" {
		t.Fatalf("the answer event %v, want via dashboard, by the owner", a)
	}
	if code, b := r.act(base, dashboard.AnswerPath, s, answer); code != http.StatusConflict || !strings.Contains(b, "no longer open") {
		t.Fatalf("the same answer again: %d %s", code, b)
	}
	r.until(t, "the answered turn", func(evs []control.Event) bool { return count(evs, "turn_ended") == 4 })

	// Stop the running turn, then the same turn again.
	if code, b := r.act(base, dashboard.SendPath, s, map[string]any{"thread": r.id, "key": "p3", "text": "tool wait 60s\nreply late"}); code != http.StatusNoContent {
		t.Fatalf("send: %d %s", code, b)
	}
	evs = r.until(t, "the tool", func(evs []control.Event) bool { return count(evs, "tool_started") == 1 })
	started := dataOf(evs, "turn_started")
	turn := started[len(started)-1]["turn"]
	if code, b := r.act(base, dashboard.StopPath, s, map[string]any{"thread": r.id, "turn": turn}); code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", code, b)
	}
	evs = r.until(t, "the stopped turn's end", func(evs []control.Event) bool { return count(evs, "turn_ended") == 5 })
	ended := dataOf(evs, "turn_ended")
	if e := ended[len(ended)-1]; e["reason"] != "interrupted" {
		t.Fatalf("the stopped turn ended %v", e)
	}
	if st := dataOf(evs, "stop"); len(st) != 1 || st[0]["via"] != "dashboard" || st[0]["principal"] != "owner" || st[0]["turn"] != turn {
		t.Fatalf("stop events %v, want one for turn %v, via dashboard, by the owner", st, turn)
	}
	if code, b := r.act(base, dashboard.SendPath, s, map[string]any{"thread": r.id, "key": "p4", "text": "tool wait 2s\nreply after"}); code != http.StatusNoContent {
		t.Fatalf("send: %d %s", code, b)
	}
	r.until(t, "the next tool", func(evs []control.Event) bool { return count(evs, "tool_started") == 2 })
	if code, b := r.act(base, dashboard.StopPath, s, map[string]any{"thread": r.id, "turn": turn}); code != http.StatusConflict || !strings.Contains(b, "no longer running") {
		t.Fatalf("a stop of the ended turn: %d %s", code, b)
	}
	evs = r.until(t, "the next turn's end", func(evs []control.Event) bool { return count(evs, "turn_ended") == 6 })
	ended = dataOf(evs, "turn_ended")
	if e := ended[len(ended)-1]; e["outcome"] != "completion" || count(evs, "stop") != 1 {
		t.Fatalf("the next turn ended %v with %d stop events: a stop of the ended turn stopped it", e, count(evs, "stop"))
	}

	// Unknown thread and request.
	if code, b := r.act(base, dashboard.SendPath, s, map[string]any{"thread": 9999, "key": "p5", "text": "reply x"}); code != http.StatusNotFound {
		t.Errorf("a send to no thread: %d %s", code, b)
	}
	if code, b := r.act(base, dashboard.AnswerPath, s, map[string]any{"request": "no-such", "text": "x"}); code != http.StatusNotFound {
		t.Errorf("an answer to no request: %d %s", code, b)
	}
	// Not from the page; not a POST.
	for _, p := range []string{dashboard.SendPath, dashboard.AnswerPath, dashboard.StopPath, dashboard.ActCheckPath, dashboard.ActVerifyPath} {
		if code, b := r.do(http.MethodPost, base, p, s, "", map[string]any{"thread": r.id, "key": "p6", "text": "reply no"}); code != http.StatusForbidden {
			t.Errorf("POST %s without Origin: %d %s", p, code, b)
		}
		if code, b := r.do(http.MethodGet, base, p, s, "", nil); code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d %s", p, code, b)
		}
	}
	if in := dataOf(r.events(t), "input"); len(in) != 5 {
		t.Fatalf("%d inputs: a refused request gave the agent input", len(in))
	}
}

// Only the owner acts from the dashboard: a person who may post in the
// scope, but is not the owner, gets no OpSend, OpAnswer or OpStop, is told
// act/may false, and each call is refused by the core and logged.
func TestDashboardActOnlyTheOwner(t *testing.T) {
	dir := people.New([]people.Person{{Name: "ada"}, {Name: "grace"}}, "ada", map[string][]string{"s": {"grace"}})
	if c := dashboardCaller(dir, []string{"s"}, false, false, "ada"); !c.May.Has(control.OpSend | control.OpAnswer | control.OpStop) {
		t.Fatalf("the owner's dashboard Caller may %v", c.May)
	}
	grace := dashboardCaller(dir, []string{"s"}, false, false, "grace")
	if grace.May.Has(control.OpSend) || grace.May.Has(control.OpAnswer) || grace.May.Has(control.OpStop) {
		t.Fatalf("a person who is not the owner gets %v", grace.May)
	}
	r := newActRig(t)
	// Only the owner may sign in today: the session is the owner's, and its
	// Caller is the one the daemon makes for grace.
	dashboardHook = func(c *control.Caller) { c.Principal, c.May = "grace", grace.May }
	t.Cleanup(func() { dashboardHook = nil })
	base, s := r.local(t)
	if code, b := r.do(http.MethodGet, base, dashboard.ActMayPath, s, "", nil); code != http.StatusOK || !strings.Contains(string(b), `"may":false`) {
		t.Fatalf("act/may for grace: %d %s", code, b)
	}
	for op, call := range map[string]struct {
		path string
		body any
	}{
		"Send":   {dashboard.SendPath, map[string]any{"thread": r.id, "key": "g1", "text": "reply grace"}},
		"Answer": {dashboard.AnswerPath, map[string]any{"request": "any", "text": "x"}},
		"Stop":   {dashboard.StopPath, map[string]any{"thread": r.id}},
	} {
		if code, b := r.act(base, call.path, s, call.body); code != http.StatusForbidden {
			t.Errorf("%s by grace: %d %s", op, code, b)
		}
		if !strings.Contains(r.logged(), fmt.Sprintf(`msg="control call refused" op=%s `, op)) {
			t.Errorf("the refusal of %s is not logged", op)
		}
	}
	if in := dataOf(r.events(t), "input"); len(in) != 1 {
		t.Fatalf("%d inputs: grace's reached the agent", len(in))
	}
}

// On the web listener a send, an answer and a stop each answer 428 until
// the session has made a passkey check through act/check and act/verify;
// then they work for five minutes, and answer 428 again after them. A check
// is the session's own, and is used once.
func TestDashboardActOnTheWebNeedsAPasskeyCheck(t *testing.T) {
	clock := useClock(t)
	r := newActRig(t)
	k := &softKey{rpID: webName, origin: r.web}
	s, reg := r.webLink()
	code, body, c := r.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("registering a passkey: %d %s", code, body)
	}
	needs := func(what string) {
		t.Helper()
		for _, call := range []struct {
			path string
			body any
		}{
			{dashboard.SendPath, map[string]any{"thread": r.id, "key": "w-" + what, "text": "reply web"}},
			{dashboard.AnswerPath, map[string]any{"request": "any", "text": "x"}},
			{dashboard.StopPath, map[string]any{"thread": r.id}},
		} {
			if code, b := r.act("", call.path, s, call.body); code != http.StatusPreconditionRequired || !strings.Contains(b, "passkey check") {
				t.Fatalf("%s: %s: %d %s, want 428", what, call.path, code, b)
			}
		}
	}
	needs("right after the sign-in")
	check := func() ceremonyAnswer {
		t.Helper()
		code, b := r.act("", dashboard.ActCheckPath, s, map[string]any{})
		var a struct {
			Needs string
			ceremonyAnswer
		}
		json.Unmarshal([]byte(b), &a)
		if code != http.StatusOK || a.Needs != "passkey" || a.Ceremony == "" {
			t.Fatalf("act/check: %d %s", code, b)
		}
		return a.ceremonyAnswer
	}
	// Another session's check does not count.
	other, _ := r.webLink()
	a := check()
	if code, b := r.act("", dashboard.ActVerifyPath, other, map[string]any{"ceremony": a.Ceremony, "credential": k.get(t, a.Options, c)}); code != http.StatusForbidden {
		t.Fatalf("another session's check: %d %s", code, b)
	}
	needs("after another session's check")
	// This session's.
	a = check()
	answer := k.get(t, a.Options, c)
	if code, b := r.act("", dashboard.ActVerifyPath, s, map[string]any{"ceremony": a.Ceremony, "credential": answer}); code != http.StatusOK || !strings.Contains(b, `"until":300`) {
		t.Fatalf("act/verify: %d %s", code, b)
	}
	if code, b := r.act("", dashboard.ActVerifyPath, s, map[string]any{"ceremony": a.Ceremony, "credential": answer}); code != http.StatusForbidden {
		t.Fatalf("the same check again: %d %s", code, b)
	}
	if code, b := r.act("", dashboard.SendPath, s, map[string]any{"thread": r.id, "key": "w1", "text": "reply checked"}); code != http.StatusNoContent {
		t.Fatalf("a send after the check: %d %s", code, b)
	}
	r.until(t, "the checked send's turn", func(evs []control.Event) bool { return count(evs, "turn_ended") == 2 })
	clock.add(dashboard.ActCheckWithin + time.Second)
	needs("five minutes after the check")
}

// On the web listener a person with no passkey acts within five minutes of
// a sign-in with a link, and not after: act/check then says to sign in
// again.
func TestDashboardActOnTheWebWithoutAPasskey(t *testing.T) {
	clock := useClock(t)
	r := newActRig(t)
	clock.add(2 * time.Second) // a sign-in in the second the server started counts as before it
	s, _ := r.webLink()
	if code, b := r.act("", dashboard.ActCheckPath, s, map[string]any{}); code != http.StatusOK || !strings.Contains(b, `"needs":"nothing"`) {
		t.Fatalf("act/check right after the sign-in: %d %s", code, b)
	}
	if code, b := r.act("", dashboard.SendPath, s, map[string]any{"thread": r.id, "key": "n1", "text": "reply fresh"}); code != http.StatusNoContent {
		t.Fatalf("a send right after the sign-in: %d %s", code, b)
	}
	clock.add(5*time.Minute + time.Second)
	if code, b := r.act("", dashboard.SendPath, s, map[string]any{"thread": r.id, "key": "n2", "text": "reply late"}); code != http.StatusPreconditionRequired {
		t.Fatalf("a send five minutes after the sign-in: %d %s", code, b)
	}
	if code, b := r.act("", dashboard.ActCheckPath, s, map[string]any{}); code != http.StatusForbidden || !strings.Contains(b, "sign in again") {
		t.Fatalf("act/check five minutes after the sign-in: %d %s", code, b)
	}
}
