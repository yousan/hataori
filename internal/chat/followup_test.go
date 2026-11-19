package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// runningThread starts a thread from a trigger and brings its session to a
// running turn, as its agent would.
func (f *front) runningThread(t *testing.T) control.ThreadID {
	t.Helper()
	id, _ := f.started(t)
	m := &session.Machine{Store: f.fe.Outbox.Store}
	if err := m.Apply(context.Background(), 1, session.Input{Kind: session.AgentEvent,
		Agent: agent.Event{Seq: 1, Kind: agent.SessionReady}}); err != nil {
		t.Fatal(err)
	}
	owner := f.fe.API(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpSend})
	if err := owner.Send(context.Background(), id, "first", control.Input{Text: "first input"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFollowUpIsDeliveredIntoTheRunningTurnOnce(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	in := f.deliver(f.conv(id), fake.Webhook(hook), "also do this")
	f.fe.Message(in) // delivered twice
	got := strings.Join(f.runner.handed(), "; ")
	if got != "deliver first input; deliver also do this" {
		t.Fatalf("handed to the agent: %s", got)
	}
	var key string
	f.fe.Outbox.Store.DB.QueryRow(`SELECT key FROM inbox WHERE key LIKE 'send:%' ORDER BY rowid DESC LIMIT 1`).Scan(&key)
	if !strings.HasSuffix(key, "\x00"+in.Ref.ID) {
		t.Fatalf("the send's key is %q, want the message id %q", key, in.Ref.ID)
	}
}

func TestFollowUpAnswersAnOpenQuestion(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	m := &session.Machine{Store: f.fe.Outbox.Store}
	if err := m.Apply(context.Background(), 1, session.Input{Kind: session.AgentEvent,
		Agent: agent.Event{Seq: 2, Kind: agent.Request, Request: &agent.RequestInfo{ID: "q1", Text: "which?"}}}); err != nil {
		t.Fatal(err)
	}
	f.deliver(f.conv(id), fake.Person(owner), "the blue one")
	if got := strings.Join(f.runner.handed(), "; "); got != "deliver first input; answer q1 the blue one" {
		t.Fatalf("handed to the agent: %s", got)
	}
}

func TestMessagesThatAreNotFollowUps(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	other := surface.Conversation{Channel: channel, Thread: "not-ours"}
	f.deliver(other, fake.Webhook(hook), "hello")
	f.deliver(other, fake.Webhook(hook), "!go repo:https://example.com/r.git do it")
	rename := f.driver.Inject(f.conv(id), fake.Person(owner), "renamed the thread")
	rename.Service = true
	f.fe.Message(rename)
	f.deliver(f.conv(id), fake.Bot("the bot"), "a post of the bot's own")
	f.deliver(f.conv(id), fake.Webhook("stranger"), "not allowed")
	if got := f.runner.handed(); len(got) != 1 {
		t.Fatalf("handed to the agent: %v", got)
	}
	if n := len(f.runner.all()); n != 1 {
		t.Fatalf("%d sessions started", n)
	}
	if c := f.driver.Calls(other); len(c) != 0 {
		t.Fatalf("calls in the thread hataori does not own: %+v", c)
	}
}
