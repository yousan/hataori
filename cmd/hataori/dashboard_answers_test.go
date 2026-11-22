package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// answersOf are the data of the thread's answer events.
func answersOf(t *testing.T, c *wire.Client, id control.ThreadID) []map[string]any {
	t.Helper()
	evs, err := c.ThreadEvents(t.Context(), id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	return dataOf(evs, "answer")
}

// The acceptance criteria of #611 in the browser, against the daemon with
// the fake surface and the fake agent on pipes: a question's choices are
// buttons, and pressing one answers it with via dashboard, the buttons
// going once it is answered; a question of several choices has checks and
// one button; while a question is open the box says that it answers it,
// and answers with Answer; an answer to a question answered elsewhere just
// before shows the server's refusal, and the buttons go.
func TestDashboardAnswersAQuestion(t *testing.T) {
	r := boxRoot(t)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" ask which colour? -- red | blue")
	x := threadOf(t, r.root)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, x.ID)

	// One choice.
	b.until("the choices as buttons", `JSON.stringify([...document.querySelectorAll('.pane.active .answers button[data-choice]')].map(b => b.dataset.choice)) === '["red","blue"]'`)
	b.click(`.pane.active .answers button[data-choice="blue"]`)
	r.posted(t, th, "answer: blue")
	b.until("the buttons gone", `!document.querySelector('.pane.active .answers')`)
	if a := answersOf(t, c, x.ID); len(a) != 1 || a[0]["via"] != "dashboard" || a[0]["principal"] != "owner" {
		t.Fatalf("answer events %v, want one via dashboard by the owner", a)
	}

	// Several choices.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "askmany which ones? -- red | green | blue")
	b.until("the checks", `document.querySelectorAll('.pane.active .answers input[type=checkbox]').length === 3 && !!document.querySelector('.pane.active .answers [data-answer-checked]')`)
	b.click(`.pane.active .answers input[data-choice="red"]`)
	b.click(`.pane.active .answers input[data-choice="blue"]`)
	b.click(`.pane.active .answers [data-answer-checked]`)
	r.posted(t, th, "answer: red, blue")
	b.until("the checks gone", `!document.querySelector('.pane.active .answers')`)

	// A free answer through the box.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "ask your name?")
	b.until("the box answering", `!!document.querySelector('.pane.active .input-box [data-box-answers]')`)
	typeInBox(b, "ada")
	b.key("Enter", "Enter", 13, 0, "\r")
	r.posted(t, th, "answer: ada")
	if a := answersOf(t, c, x.ID); len(a) != 3 || a[2]["text"] != "ada" || a[2]["via"] != "dashboard" {
		t.Fatalf("answer events %v, want the box's as an answer via dashboard", a)
	}
	if in := inputsOf(t, c, x.ID); strings.Contains(fmt.Sprint(in), "ada") {
		t.Fatalf("the box's answer went as input: %v", in)
	}

	// Answered elsewhere just before: the refusal, and the buttons go.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "ask which side? -- left | right")
	b.until("the new choices", `!!document.querySelector('.pane.active .answers button[data-choice="left"]')`)
	d, err := c.Thread(context.Background(), x.ID, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("the open question: %+v %v", d.Requests, err)
	}
	if err := c.Answer(context.Background(), d.Requests[0].ID, control.Answer{Choices: []string{"right"}}); err != nil {
		t.Fatal(err)
	}
	b.click(`.pane.active .answers button[data-choice="left"]`)
	b.until("the refusal, the buttons gone", `(document.querySelector('.pane.active [data-answer-refused]')?.textContent ?? '').startsWith('Not answered: the question is no longer open') && !document.querySelector('.pane.active .answers')`)
	r.posted(t, th, "answer: right")

	// The box's answer to a question answered elsewhere just before is
	// refused too, and is not taken as a new message.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "ask your city?")
	b.until("the box answering again", `!!document.querySelector('.pane.active .input-box [data-box-answers]')`)
	typeInBox(b, "zed")
	d, err = c.Thread(context.Background(), x.ID, 0, 0)
	if err != nil || len(d.Requests) != 1 {
		t.Fatalf("the open question: %+v %v", d.Requests, err)
	}
	if err := c.Answer(context.Background(), d.Requests[0].ID, control.Answer{Text: "paris"}); err != nil {
		t.Fatal(err)
	}
	b.key("Enter", "Enter", 13, 0, "\r")
	b.until("the box's refusal", `(document.querySelector('.pane.active [data-box-refused]')?.textContent ?? '').includes('no longer open') && document.querySelector('.input-box textarea').value === 'zed'`)
	r.posted(t, th, "answer: paris")
	time.Sleep(300 * time.Millisecond)
	for _, in := range inputsOf(t, c, x.ID) {
		if in["text"] == "zed" {
			t.Fatal("the box's refused answer went to the agent as a new message")
		}
	}
}

// A terminal thread in the wide layout has no box: its question's free
// answer goes through a field under the buttons. A person who may not act
// sees no buttons and no field.
func TestDashboardAnswersInATerminalThread(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" ask your name?")
	x := threadOf(t, r.root)
	eventually(t, "the question", 20*time.Second, func() bool { return threadOf(t, r.root).State == control.Waiting })
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1400, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, x.ID)
	b.until("the answer field", `!!document.querySelector('.pane.active .answers [data-answer-field]') && !document.querySelector('.pane.active .input-box')`)
	b.eval(`document.querySelector('.pane.active [data-answer-field]').focus(), true`, nil)
	b.call("Input.insertText", map[string]any{"text": "grace"})
	b.key("Enter", "Enter", 13, 0, "\r")
	r.posted(t, th, "answer: grace")

	dashboardHook = func(c *control.Caller) { c.Principal, c.May = "grace", control.OpRead }
	t.Cleanup(func() { dashboardHook = nil })
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "ask again? -- yes | no")
	eventually(t, "the second question", 20*time.Second, func() bool { return threadOf(t, r.root).State == control.Waiting })
	b2 := newBrowser(t)
	b2.call("Page.enable", map[string]any{})
	b2.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b2, x.ID)
	time.Sleep(time.Second)
	if n := countOf(b2, ".pane.active .answers, .pane.active [data-answer-field], .pane.active .input-box"); n != 0 {
		t.Fatalf("a person who may not act sees %d ways to answer", n)
	}
}

// The approvals a supervisor asks for, in its own thread, each have Allow
// and Deny, all of them shown at once; each answers its own, with via
// dashboard.
func TestDashboardAnswersAnApproval(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
			{"name": "sv", "ids": []string{"supervisor:c1"}},
		}
		cfg["scopes"] = []map[string]any{{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch", "sv"}}}
	})
	var c *core.Core
	coreHook = func(got *core.Core) { c = got }
	t.Cleanup(func() { coreHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	asker := r.order(t, 0, "!go repo:"+repo+" reply the asker")
	r.posted(t, asker, "the asker")
	r.order(t, 1, "!go repo:"+repo+" tool wait 60s\nreply late")
	r.order(t, 2, "!go repo:"+repo+" tool wait 60s\nreply later")
	ctx := context.Background()
	owner := c.As(control.Caller{Principal: "ada", Scopes: []string{"c1"}, May: control.OpRead})
	var askerID control.ThreadID
	var targets []control.ThreadID
	eventually(t, "the targets running", 20*time.Second, func() bool {
		ts, _ := owner.Threads(ctx, control.Filter{})
		askerID, targets = 0, nil
		for _, x := range ts {
			if strings.Contains(x.Title, "asker") {
				askerID = x.ID
			} else if x.State == control.Running {
				targets = append(targets, x.ID)
			}
		}
		return askerID != 0 && len(targets) == 2
	})
	// Two approvals at once, in the supervisor's thread: both shown.
	sv := c.As(control.Caller{From: "supervisor:c1", Scopes: []string{"c1"}, May: control.OpRead | control.OpAsk, AsksIn: askerID})
	for i, target := range targets {
		if _, err := sv.AskInterrupt(ctx, target, fmt.Sprintf("k%d", i), "stop it"); err != nil {
			t.Fatal(err)
		}
	}
	b := newBrowser(t)
	b.call("Page.enable", map[string]any{})
	b.call("Page.navigate", map[string]any{"url": signinLink(t, r.root)})
	openThread(t, b, askerID)
	b.until("two approvals, each with Allow and Deny", `document.querySelectorAll('.pane.active .answers[data-approval=yes] [data-allow=yes]').length === 2 && document.querySelectorAll('.pane.active .answers [data-allow=no]').length === 2`)
	b.click(`.pane.active .answers [data-allow=no]`)
	b.until("one approval left", `document.querySelectorAll('.pane.active .answers').length === 1`)
	b.click(`.pane.active .answers [data-allow=yes]`)
	b.until("the approvals gone", `!document.querySelector('.pane.active .answers')`)
	var a []map[string]any
	eventually(t, "both decisions", 10*time.Second, func() bool {
		evs, err := owner.ThreadEvents(ctx, askerID, 0, control.MaxEvents)
		if err != nil {
			t.Fatal(err)
		}
		a = dataOf(evs, "approval_answered")
		return len(a) == 2
	})
	if a[0]["allow"] != false || a[1]["allow"] != true || a[0]["via"] != "dashboard" || a[1]["via"] != "dashboard" {
		t.Fatalf("approval_answered %v, want one refused and one allowed, via dashboard", a)
	}
}
