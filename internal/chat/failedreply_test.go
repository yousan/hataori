package chat

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// failedThread is a thread whose agent ended at its start, with its "could
// not start" message posted, and that message.
func failedThread(t *testing.T, f *front, e agent.Event) (control.ThreadID, fake.Call) {
	t.Helper()
	id, _ := f.started(t)
	f.showing(t)
	e.Seq, e.Kind, e.Reason = 1, agent.SessionEnded, agent.BlockedAtStart
	if err := f.core.Machine.Apply(context.Background(), f.runner.all()[0].Session, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
		t.Fatal(err)
	}
	var msg fake.Call
	waitFor(t, "the message of the failure", func() bool {
		calls := f.postsOf(f.conv(id))
		if len(calls) == 2 {
			msg = calls[1]
		}
		return len(calls) == 2
	})
	return id, msg
}

// postsOf are the posts in conv, without settled's marker.
func (f *front) postsOf(conv surface.Conversation) (posts []fake.Call) {
	for _, c := range f.driver.Calls(conv) {
		if c.Op == fake.OpPost && c.Message.Text != "marker" {
			posts = append(posts, c)
		}
	}
	return posts
}

// A message in a thread that failed at its start reaches no agent and no
// plugin, and gets one reply that says so, why the thread could not start
// and the next step, also when it is delivered twice; a guest's message and
// a bot's get nothing (#708).
func TestMessageInAFailedThreadGetsOneReply(t *testing.T) {
	f := newFront(t, workspaces{})
	h := &script{}
	f.core.Hooks = h
	id, msg := failedThread(t, f, agent.Event{Text: "no such model"})
	th := f.conv(id)
	asked := len(h.got())

	f.deliver(th, fake.Person("guest-1"), "let me in")
	f.deliver(th, fake.Bot("bot-1"), "a notice")
	in := f.deliver(th, fake.Webhook(hook), "are you there")
	f.fe.Message(in)
	f.settled(t, th)
	want := "the prompt|" + msg.Message.Text + "|" + FailedLine + " " + msg.Message.Text + "\n" + NewThreadLine
	if got := texts(f.postsOf(th)); got != want {
		t.Fatalf("thread shows %q, want %q", got, want)
	}
	if r := f.postsOf(th)[2].Message.ReplyTo; r == nil || r.ID != in.Ref.ID {
		t.Fatalf("the reply is to %v, want %s", r, in.Ref.ID)
	}
	if len(f.runner.handed()) != 0 || len(f.runner.all()) != 1 || len(h.got()) != asked {
		t.Fatalf("handed over %q, %d starts, hooks %q", f.runner.handed(), len(f.runner.all()), h.got())
	}
	// Another message gets its own.
	f.deliver(th, fake.Webhook(hook), "still there?")
	waitFor(t, "the second reply", func() bool { return len(f.postsOf(th)) == 4 })
}

// The message that woke the thread was taken: delivered again once the wake
// failed, it is not said not to have been.
func TestAMessageTakenBeforeTheFailureGetsNoReply(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := failedThread(t, f, agent.Event{Text: "no such model"})
	th := f.conv(id)
	in := f.driver.Inject(th, fake.Webhook(hook), "go on")
	if _, err := f.fe.Outbox.Store.DB.Exec(`INSERT INTO inbox (key, data, accepted_at) VALUES (?, '{}', 0)`,
		"send:"+channel+"\x00"+in.Ref.ID); err != nil {
		t.Fatal(err)
	}
	for _, hooks := range []control.Hooks{nil, &script{}} {
		f.core.Hooks = hooks
		f.fe.Message(in)
	}
	f.settled(t, th)
	if n := len(f.postsOf(th)); n != 2 {
		t.Fatalf("%d posts, want 2", n)
	}
	if f.storedCursor(th) == "" {
		t.Fatal("the message was not handled")
	}
}

// The reply in a thread whose start files blocked offers the choices of the
// message (#590). A choice takes them away from the reply too, in the link
// it was posted in, and the message that was answered is not sent to the
// thread started again when it is delivered once more.
func TestTheReplyInABlockedThreadOffersTheChoices(t *testing.T) {
	for _, second := range []bool{false, true} {
		f := newFront(t, workspaces{})
		id, msg := failedThread(t, f, agent.Event{Text: "the dialog", Trust: []string{".agent/settings.json"}})
		th := f.conv(id)
		if second {
			th = f.secondLink(t, id, "t-second")
		}
		before := len(f.postsOf(th))
		in := f.deliver(th, fake.Webhook(hook), "are you there")
		waitFor(t, "the reply", func() bool { return len(f.postsOf(th)) == before+1 })
		reply := f.postsOf(th)[before]
		if reply.Message.Text != FailedLine+" "+msg.Message.Text || !slices.Equal(reply.Message.Choices, msg.Message.Choices) || len(reply.Message.Choices) != 3 {
			t.Fatalf("reply %q with %v, want the message's text and choices %v", reply.Message.Text, reply.Message.Choices, msg.Message.Choices)
		}
		f.fe.Choice(surface.Selection{Ref: reply.Ref, Author: fake.Webhook(hook), Token: reply.Message.Choices[0].Token})
		var edit string
		waitFor(t, "the reply to lose its buttons", func() bool {
			for _, c := range f.driver.Calls(th) {
				if c.Op == fake.OpEdit && c.Ref == reply.Ref && len(c.Message.Choices) == 0 {
					edit = c.Message.Text
				}
			}
			return edit != ""
		})
		if !strings.HasPrefix(edit, FailedLine+" "+couldNotStart) || !strings.HasSuffix(edit, "\nStarted without this repository's settings by hook.") {
			t.Fatalf("the reply reads %q", edit)
		}
		if n := len(f.runner.all()); n != 2 {
			t.Fatalf("%d starts, want the thread started again", n)
		}
		f.fe.Message(in)
		f.settled(t, th)
		time.Sleep(50 * time.Millisecond)
		var inputs int
		f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM inbox WHERE key = ?`, "send:"+channel+"\x00"+in.Ref.ID).Scan(&inputs)
		if got := f.runner.handed(); inputs != 0 || len(got) != 0 {
			t.Fatalf("the answered message was sent: %q", got)
		}
	}
}

// A choice made after the core refused the message and before its reply is
// written: no reply, whose buttons nothing would take away, and the message
// is not taken as answered.
func TestAChoiceMadeBeforeTheReplyIsWritten(t *testing.T) {
	f := newFront(t, workspaces{})
	id, msg := failedThread(t, f, agent.Event{Text: "the dialog", Trust: []string{".agent/settings.json"}})
	th := f.conv(id)
	ctx := context.Background()
	in := f.driver.Inject(th, fake.Webhook(hook), "are you there")
	f.fe.Choice(surface.Selection{Ref: msg.Ref, Author: fake.Webhook(hook), Token: msg.Message.Choices[0].Token})
	waitFor(t, "the start", func() bool { return len(f.runner.all()) == 2 })

	if err := f.fe.failedReply(ctx, in, control.ThreadInfo{ID: id, Scope: channel, State: control.Failed}); !errors.Is(err, errMovedOn) {
		t.Fatalf("failedReply: %v, want errMovedOn", err)
	}
	if done, err := f.fe.failedAnswered(ctx, in); done || err != nil {
		t.Fatalf("answered %v, %v", done, err)
	}
	f.settled(t, th)
	if n := len(f.postsOf(th)); n != 2 {
		t.Fatalf("%d posts, want 2", n)
	}
}
