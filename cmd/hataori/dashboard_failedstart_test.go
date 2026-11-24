package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// blockedAdapters are the adapters of an instance whose fake agent, in a
// terminal, asks at its start about a settings file nobody trusted, on a
// repository with a hook of its own (fake.Config.ProjectHook).
func blockedAdapters() map[string]any {
	return map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
		Env: []string{"PATH=" + os.Getenv("PATH")}, Terminal: true, Hataori: os.Args[0],
		Dialog: "Do you trust the files in this folder? Enter to go on", DialogTrust: []string{".claude/settings.json"},
		ProjectHook: true}}
}

// blockedRoot is a chat root with those adapters, whose owner is ada and
// whose scope's poster is dispatch.
func blockedRoot(t *testing.T) *chatRoot {
	t.Helper()
	return newChatRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = blockedAdapters()
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
		}
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch"}}}
	})
}

// failedStart is a thread's failed start as the page shows it.
const (
	failedStart  = `.pane.active [data-failed-start]`
	startChoices = `.pane.active [data-start-choice]`
)

// idOf is th's thread's number.
func idOf(t *testing.T, r *chatRoot, th surface.Conversation) control.ThreadID {
	t.Helper()
	n, err := strconv.ParseInt(numberOf(t, r.root, th), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return control.ThreadID(n)
}

// startField is the text of a field of the failed start shown.
func startField(name string) string {
	return fmt.Sprintf(`(document.querySelector('%s [data-field=%s] dd')?.textContent ?? '')`, failedStart, name)
}

// showView shows the active pane's view.
func (b *browser) showView(view string) {
	b.t.Helper()
	b.click(`.pane.active .view-switch [data-view=` + view + `]`)
	b.until("the "+view, `document.querySelector('.pane.active .thread-pane')?.dataset.view === `+js(view))
}

// The acceptance criteria of #680 for a start that the folder-trust dialog
// blocked, in the browser, against the daemon with the fake surface and the
// fake agent in a terminal, with a project hook. The pane says in both
// views what the thread was asked, who started it and why it stopped, in
// one sentence; the details panel has the full reason, folded. The owner
// has the three choices the chat has, and each does what the chat's button
// does: once one is taken, the pane and the chat say who chose what, and
// the chat's buttons go.
func TestDashboardChoosesForABlockedStart(t *testing.T) {
	r := blockedRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1500, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	blocked := func(n int, repo, reply string) (surface.Conversation, fakesurface.Call, control.ThreadID) {
		t.Helper()
		r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+repo+" reply "+reply)
		th := r.thread(t, n)
		msg := r.blockedMessage(t, th)
		id := idOf(t, r, th)
		openThread(t, b, id)
		b.until("the three choices", `document.querySelectorAll('`+startChoices+`').length === 3`)
		return th, msg, id
	}
	chose := func(id control.ThreadID, choice string) {
		t.Helper()
		evs, err := c.ThreadEvents(context.Background(), id, 0, control.MaxEvents)
		if err != nil {
			t.Fatal(err)
		}
		got := dataOf(evs, "start_chosen")
		if len(got) != 1 || got[0]["choice"] != choice || got[0]["principal"] != "ada" || got[0]["via"] != "dashboard" {
			t.Fatalf("start_chosen %v, want one: %s by ada, via dashboard", got, choice)
		}
	}
	says := func(line string) {
		t.Helper()
		b.showView("terminal")
		b.until("who chose what, in the terminal view", `document.querySelector('.pane.active [data-start-chosen]')?.textContent === `+js(line))
		b.showView("conversation")
		b.until("who chose what, in the conversation", `document.querySelector('.pane.active li[data-kind=start_chosen]')?.textContent.startsWith(`+js(line)+`)`)
	}

	// 1. The statement, in both views; then without the repository's
	// settings.
	th, msg, id := blocked(0, bareRepo(t), "without")
	for _, view := range []string{"terminal", "conversation"} {
		b.showView(view)
		b.until("what it was asked, in the "+view, startField("asked")+` === 'reply without'`)
		b.until("who started it, in the "+view, startField("by")+` === 'dispatch'`)
		b.until("why it stopped, in the "+view, startField("why")+` === "Its agent asked whether to trust the repository's own settings (.claude/settings.json), and nobody has trusted them for this channel yet."`)
		b.until("the choices, in the "+view, `JSON.stringify([...document.querySelectorAll('`+startChoices+`')].map(e => e.dataset.startChoice + '/' + e.textContent)) === `+
			js(`["without project settings/Start without this repository's settings","trusted/Trust and start","closed/Do not start"]`))
	}
	b.until("the full reason folded in the details panel", `(() => {
		const r = document.querySelector('.details [data-block=failure] details:not([open]) [data-field=reason]');
		return !!r && r.textContent.includes('this session may not trust them') && !r.checkVisibility() && !document.querySelector('.pane.active').textContent.includes('may not trust them');
	})()`)
	b.click(startChoices + `[data-start-choice="without project settings"]`)
	r.posted(t, th, "without")
	r.edited(t, th, msg, "Started without this repository's settings by ada.")
	if r.hookRan(t, th) {
		t.Fatal("the repository's hook ran in a thread started without its settings")
	}
	chose(id, "without project settings")
	says("Started without this repository's settings by ada")
	b.until("the failed start gone", `!document.querySelector('`+failedStart+`')`)

	// 2. Trust and start: recorded as the chat's button records it.
	repo := bareRepo(t)
	th, msg, id = blocked(1, repo, "trusting")
	b.click(startChoices + `[data-start-choice="trusted"]`)
	r.posted(t, th, "trusting")
	r.edited(t, th, msg, "Trusted by ada for this channel's threads on "+repo+": starting again.")
	if n := r.trusted(t); n != 1 {
		t.Fatalf("%d files trusted, want 1", n)
	}
	if !r.hookRan(t, th) {
		t.Fatal("the repository's hook did not run in the trusted thread: the check above proves nothing")
	}
	chose(id, "trusted")
	says("The repository's settings were trusted, and the thread started, by ada")

	// 3. Do not start: the thread is closed, its statement stays.
	th, msg, id = blocked(2, bareRepo(t), "never")
	b.click(startChoices + `[data-start-choice="closed"]`)
	eventually(t, "the thread to close", r.within(10*time.Second), func() bool { return r.state(t, th) == "closed" })
	r.edited(t, th, msg, "Not started: closed by ada.")
	chose(id, "closed")
	says("Not started: closed by ada")
	b.until("the statement without the choices", `!!document.querySelector('`+failedStart+`') && !document.querySelector('`+startChoices+`')`)
	b.until("what the closed thread was asked", startField("asked")+` === 'reply never'`)

	// 4. A choice taken in the chat shows in the pane.
	th, msg, _ = blocked(3, bareRepo(t), "elsewhere")
	if err := r.driver.Press(msg.Ref, e2eHook, choiceToken(t, msg, "Start without this repository's settings")); err != nil {
		t.Fatal(err)
	}
	r.posted(t, th, "elsewhere")
	r.edited(t, th, msg, "Started without this repository's settings by dispatch.")
	says("Started without this repository's settings by dispatch")
}

// A person who is not the owner reads the statement and sees no choices.
func TestDashboardBlockedStartOfANonOwner(t *testing.T) {
	r := blockedRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply never")
	th := r.thread(t, 0)
	r.blockedMessage(t, th)
	dashboardHook = func(c *control.Caller) { c.Principal, c.Scopes, c.May = "dispatch", []string{"c1"}, control.OpRead }
	t.Cleanup(func() { dashboardHook = nil })
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, idOf(t, r, th))
	b.until("the statement", startField("asked")+` === 'reply never'`)
	time.Sleep(time.Second)
	if n := countOf(b, startChoices); n != 0 {
		t.Fatalf("a person who is not the owner sees %d choices", n)
	}
}

// A thread that the channel's supervisor started says so, with a link that
// opens the supervisor's thread; one whose agent could not be started says
// that, and offers no choices.
func TestDashboardFailedStartBySupervisor(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "fail")
	repo := bareRepo(t)
	r, _ := svRoot(t, true, autonomy("act"), func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["repos"] = []string{repo}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		ad := cfg["adapters"].(map[string]any)[fake.Name].(fake.Config)
		ad.Command = []string{"sh", "-c", `flag=$1; shift; if [ -e "$flag" ]; then exit 3; fi; exec "$@"`, "sh", flag, os.Args[0], "fake"}
		cfg["adapters"] = map[string]any{fake.Name: ad}
	})
	issued := make(chan func(string) (string, error), 1)
	supervisorHook = func(i func(string) (string, error), _ func(string)) { issued <- i }
	t.Cleanup(func() { supervisorHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	sv := supervisorReady(t, owner, "c1")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := svConn(t, r.root, <-issued).Start(context.Background(), control.StartRequest{Key: "w", Scope: "c1", Thread: "w", Repo: repo,
		Input: control.Input{Text: "reply the work"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread to fail", 20*time.Second, func() bool {
		d, err := owner.Thread(context.Background(), id, 0, 0)
		return err == nil && d.State == control.Failed
	})
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1500, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	b.showAll()
	openThread(t, b, id)
	// The supervisor's input starts with a line that says where it is from.
	b.until("what it was asked", startField("asked")+`.endsWith('reply the work')`)
	b.until("who started it", startField("by")+fmt.Sprintf(` === "the channel's supervisor (#%d)"`, sv.ID))
	b.until("why it stopped", startField("why")+` === 'Its agent ended as it started.'`)
	b.until("who started it, in the details panel", fmt.Sprintf(`document.querySelector('.details [data-field=startedBy] dd')?.textContent === "the channel's supervisor (#%d)"`, sv.ID))
	if n := countOf(b, startChoices); n != 0 {
		t.Fatalf("a start that no files blocked offers %d choices", n)
	}
	b.click(fmt.Sprintf(`%s [data-supervisor="%d"]`, failedStart, sv.ID))
	b.until("the supervisor's thread", fmt.Sprintf(`!!document.querySelector('.pane.active[data-tab="thread:%d"] .thread-pane')`, sv.ID))
}

// The choices' route: each choice goes through the control operation with
// its checks, and on the web listener only with the step-up that typing
// needs. A person who is not the owner is refused, each call logged; a
// choice that the thread does not wait for, or of an unknown kind, changes
// nothing.
func TestDashboardChooseRoute(t *testing.T) {
	clock := useClock(t)
	d := newPasskeyDash(t)
	setConfig(t, d.root, func(cfg map[string]any) { cfg["adapters"] = blockedAdapters() })
	logged := startDaemonLog(t, d.root)
	c, err := wire.Dial(d.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	start := func(key string) (control.ThreadID, control.FailedStart) {
		t.Helper()
		id, err := c.Start(ctx, control.StartRequest{Key: key, Scope: "s", Thread: key, Repo: bareRepo(t), Input: control.Input{Text: "reply " + key}})
		if err != nil {
			t.Fatal(err)
		}
		var f control.FailedStart
		eventually(t, "the blocked start", 20*time.Second, func() bool {
			x, err := c.Thread(ctx, id, 0, 0)
			if err != nil || x.FailedStart == nil {
				return false
			}
			f = *x.FailedStart
			return true
		})
		if len(f.Trust) != 1 || f.Text != "reply "+key {
			t.Fatalf("the failed start %+v", f)
		}
		return id, f
	}
	r := &actRig{passkeyDash: d, c: c, logged: logged}
	body := func(id control.ThreadID, f control.FailedStart, choice string) map[string]any {
		return map[string]any{"thread": id, "session": f.Session, "choice": choice, "files": f.Trust}
	}
	still := func(what string, id control.ThreadID) {
		t.Helper()
		if x, err := c.Thread(ctx, id, 0, 0); err != nil || x.State != control.Failed {
			t.Fatalf("%s: the thread is %v %v, want failed", what, x.State, err)
		}
	}
	choices := []string{"without project settings", "trusted", "closed"}

	id, f := start("one")
	base, s := r.local(t)
	for what, bad := range map[string]map[string]any{
		"no thread":  {"session": f.Session, "choice": "closed"},
		"no session": {"thread": id, "choice": "closed"},
		"no choice":  {"thread": id, "session": f.Session, "choice": "later"},
	} {
		if code, b := r.act(base, dashboard.ChoosePath, s, bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", what, code, b)
		}
	}
	for _, choice := range choices {
		stale := body(id, f, choice)
		stale["session"] = f.Session + 1000
		if code, b := r.act(base, dashboard.ChoosePath, s, stale); code != http.StatusConflict {
			t.Errorf("%s for another session: %d %s", choice, code, b)
		}
		if code, b := r.do(http.MethodPost, base, dashboard.ChoosePath, s, "", body(id, f, choice)); code != http.StatusForbidden {
			t.Errorf("%s without the page's Origin: %d %s", choice, code, b)
		}
	}
	other := body(id, f, "trusted")
	other["files"] = []string{"another file"}
	if code, b := r.act(base, dashboard.ChoosePath, s, other); code != http.StatusConflict {
		t.Errorf("trusting another file: %d %s", code, b)
	}
	if code, b := r.act(base, dashboard.ChoosePath, s, map[string]any{"thread": 9999, "session": 1, "choice": "closed"}); code != http.StatusNotFound {
		t.Errorf("no such thread: %d %s", code, b)
	}
	still("after the refused requests", id)

	// The web listener: the same step-up as a send.
	clock.add(2 * time.Second) // a sign-in in the second the server started counts as before it
	web, _ := r.webLink()
	clock.add(5*time.Minute + time.Second)
	for _, choice := range choices {
		if code, b := r.act("", dashboard.ChoosePath, web, body(id, f, choice)); code != http.StatusPreconditionRequired {
			t.Errorf("%s on the web listener with no check: %d %s, want 428", choice, code, b)
		}
	}
	still("after the refused requests on the web listener", id)

	// Not the owner: refused by the core, and logged.
	dashboardHook = func(c *control.Caller) { c.Principal, c.Scopes, c.May = "grace", []string{"s"}, control.OpRead }
	t.Cleanup(func() { dashboardHook = nil })
	for choice, op := range map[string]string{"without project settings": "StartWithoutSettings", "trusted": "Trust", "closed": "CloseThread"} {
		if code, b := r.act(base, dashboard.ChoosePath, s, body(id, f, choice)); code != http.StatusForbidden {
			t.Errorf("%s by grace: %d %s", choice, code, b)
		}
		if !strings.Contains(logged(), `msg="control call refused" op=`+op+` `) {
			t.Errorf("the refusal of %s is not logged", op)
		}
	}
	still("after a person who is not the owner", id)
	dashboardHook = nil

	// A fresh sign-in on the web listener takes one; then none is open.
	web, _ = r.webLink()
	if code, b := r.act("", dashboard.ChoosePath, web, body(id, f, "closed")); code != http.StatusNoContent {
		t.Fatalf("closed, right after the sign-in: %d %s", code, b)
	}
	for _, choice := range choices {
		if code, b := r.act(base, dashboard.ChoosePath, s, body(id, f, choice)); code != http.StatusConflict || !strings.Contains(b, "no longer waits") {
			t.Errorf("%s after the thread was closed: %d %s", choice, code, b)
		}
	}
}
