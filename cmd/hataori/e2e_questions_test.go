package main

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #257: a question from the fake agent is shown
// with one choice per option, and a press answers it.

const colours = "Which colour? -- red | green | blue"

// question waits for the question in th, posted with n choices.
func (p *procRoot) question(t *testing.T, th surface.Conversation, n int) fakesurface.Call {
	t.Helper()
	var q fakesurface.Call
	eventually(t, "the question with its choices", 15*time.Second, func() bool {
		for _, c := range p.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) > 0 {
				q = c
				return true
			}
		}
		return false
	})
	if len(q.Message.Choices) != n {
		t.Fatalf("the question has the choices %+v, want %d", q.Message.Choices, n)
	}
	return q
}

// choiceToken returns the token of the choice labelled label.
func choiceToken(t *testing.T, q fakesurface.Call, label string) string {
	t.Helper()
	for _, c := range q.Message.Choices {
		if c.Label == label {
			return c.Token
		}
	}
	t.Fatalf("no choice %q in %+v", label, q.Message.Choices)
	return ""
}

// asked starts a thread whose agent asks script, and returns the chat
// thread and the question shown there with n choices.
func (p *procRoot) asked(t *testing.T, script string, n int) (surface.Conversation, fakesurface.Call) {
	t.Helper()
	p.in(c1, "!go repo:"+bareRepo(t)+" "+script)
	th := p.thread(t, 0)
	return th, p.question(t, th, n)
}

// pressed presses as who, once the daemon is attached to the service.
func (p *procRoot) pressed(t *testing.T, q fakesurface.Call, who string, tokens ...string) {
	t.Helper()
	eventually(t, "the daemon to be attached", 15*time.Second, p.svc.isAttached)
	if err := p.driver.PressMany(q.Ref, who, tokens...); err != nil {
		t.Fatal(err)
	}
}

// pressedStale delivers a press of q as who even when the message no
// longer shows the choice, as a client with an old view sends it.
func (p *procRoot) pressedStale(t *testing.T, q fakesurface.Call, who string, tokens ...string) {
	t.Helper()
	eventually(t, "the daemon to be attached", 15*time.Second, p.svc.isAttached)
	p.svc.Choice(surface.Selection{Ref: q.Ref, Author: fakesurface.Person(who), Token: tokens[0], Tokens: tokens})
}

// disarmed waits until the question's message is edited to lose its
// choices and to say want.
func (p *procRoot) disarmed(t *testing.T, th surface.Conversation, q fakesurface.Call, want string) {
	t.Helper()
	eventually(t, "the question to lose its choices", 15*time.Second, func() bool {
		for _, c := range p.driver.Calls(th) {
			if c.Op == fakesurface.OpEdit && c.Ref == q.Ref && len(c.Message.Choices) == 0 && strings.Contains(c.Message.Text, want) {
				return true
			}
		}
		return false
	})
}

func (p *procRoot) logged() string {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	return p.log.String()
}

func TestQuestionWithButtonsIsAnsweredByAPress(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	th, q := p.asked(t, "ask "+colours, 3)
	if q.Message.Text != "Which colour?" || q.Message.Multi {
		t.Fatalf("question %+v", q.Message)
	}
	p.pressed(t, q, e2eOwner, choiceToken(t, q, "green"))
	p.posted(t, th, "answer: green") // the turn went on with the option
	p.disarmed(t, th, q, "Answered: green")
}

func TestQuestionIsAnsweredByAPressAfterARestart(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	th, q := p.asked(t, "ask "+colours, 3)
	p.restart(t)
	p.pressed(t, q, e2eOwner, choiceToken(t, q, "blue"))
	p.posted(t, th, "answer: blue")
}

func TestMultiSelectQuestionDeliversTwoOptions(t *testing.T) {
	p := newProcRoot(t, nil)
	l := fakesurface.DefaultLimits
	l.MultiSelect = true
	p.driver.SetLimits(l)
	p.start(t)
	th, q := p.asked(t, "askmany "+colours, 3)
	if !q.Message.Multi {
		t.Fatalf("question %+v, want several choices at once", q.Message)
	}
	p.pressed(t, q, e2eOwner, choiceToken(t, q, "red"), choiceToken(t, q, "blue"))
	p.posted(t, th, "answer: red, blue")
}

// A guest, and a person allowed in no scope.
func TestPressOfAGuestOrAnOutsiderDeliversNothing(t *testing.T) {
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{remoteSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{remoteSurface + ":" + e2eHook}},
			{"name": "grace", "ids": []string{remoteSurface + ":g-1"}},
		}
		sc := cfg["scopes"].([]map[string]any)[0]
		delete(sc, "posters")
		sc["people"] = []string{"dispatch"}
	})
	p.start(t)
	th, q := p.asked(t, "ask "+colours, 3)
	for _, who := range []string{"x-9", "g-1"} {
		before := strings.Count(p.logged(), "id="+who)
		p.pressed(t, q, who, choiceToken(t, q, "red"))
		eventually(t, "the refusal in the log", 5*time.Second, func() bool { return strings.Count(p.logged(), "id="+who) > before })
		time.Sleep(200 * time.Millisecond)
		if n := strings.Count(p.logged(), "id="+who) - before; n != 1 {
			t.Fatalf("%s's press: %d log lines with the id, want 1:\n%s", who, n, p.logged())
		}
	}
	if a := p.events(t, "answer", th); len(a) != 0 {
		t.Fatalf("answers %v, want none", a)
	}
	// The question is still open: the owner's press answers it.
	p.pressed(t, q, e2eOwner, choiceToken(t, q, "green"))
	p.posted(t, th, "answer: green")
}

// A second press after the answer, and a press after the request closed,
// deliver nothing.
func TestPressAfterTheAnswerOrTheCloseDeliversNothing(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	th, q := p.asked(t, "ask "+colours+"\nask Which shape? -- round | square", 3)
	p.pressed(t, q, e2eOwner, choiceToken(t, q, "red"))
	eventually(t, "the answer", 15*time.Second, func() bool { return len(p.events(t, "answer", th)) == 1 })
	// The second question is asked in the same turn, after the answer.
	var q2 fakesurface.Call
	eventually(t, "the second question", 15*time.Second, func() bool {
		for _, c := range p.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 2 {
				q2 = c
				return true
			}
		}
		return false
	})
	// A second press of the first question, while the second is open.
	p.pressedStale(t, q, e2eOwner, choiceToken(t, q, "blue"))
	time.Sleep(500 * time.Millisecond)
	if a := p.events(t, "answer", th); len(a) != 1 {
		t.Fatalf("answers %v after the second press, want the first only", a)
	}
	// Its agent goes: the request closes with it.
	if err := syscall.Kill(p.agentPID(t), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the session to end", 15*time.Second, func() bool {
		var open int
		p.db(t).QueryRow(`SELECT count(*) FROM requests WHERE state = 'open'`).Scan(&open)
		return open == 0
	})
	p.pressedStale(t, q2, e2eOwner, choiceToken(t, q2, "round"))
	time.Sleep(500 * time.Millisecond)
	if a := p.events(t, "answer", th); len(a) != 1 || a[0]["text"] != "red" {
		t.Fatalf("answers %v, want the first press's only", a)
	}
	if n := posts(p.driver, th, "answer: blue"); n != 0 {
		t.Fatalf("the second press was delivered")
	}
}

func TestTypedMessageStillAnswersAnOpenQuestion(t *testing.T) {
	p := newProcRoot(t, nil)
	p.start(t)
	th, q := p.asked(t, "ask "+colours, 3)
	p.in(th, "green")
	p.posted(t, th, "answer: green")
	p.disarmed(t, th, q, "Answered: green")
}
