package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

func conversationsFront(t *testing.T) *front {
	t.Helper()
	f := newFront(t, workspaces{})
	sv := &supervisor.Sessions{Core: f.core, Root: t.TempDir(), Log: f.fe.Log,
		Scopes: map[string]supervisor.Scope{channel: {Conversations: true}}}
	f.fe.Scopes[channel] = Scope{Enabled: true, Conversations: true}
	f.fe.CheckCall = func(scope, from string) error {
		return f.core.CheckConversationCall(control.Caller{From: from, Scopes: []string{scope}, May: control.OpStart}, scope)
	}
	f.fe.Retry = func(ctx context.Context, scope string, id control.ThreadID, key, from string, in control.Input) error {
		return f.core.RetryConversation(ctx, control.Caller{From: from, Scopes: []string{scope}, May: control.OpRead | control.OpStart}, id, key, in)
	}
	f.fe.Converse = func(ctx context.Context, scope, thread, key, from, text string) (control.ThreadID, error) {
		return sv.Converse(ctx, supervisor.Call{Scope: scope, Thread: thread, Key: key, From: from, Text: text})
	}
	return f
}

func callMessage(conv surface.Conversation, key string, author surface.Author, text string) surface.Inbound {
	const mention = "<@self>"
	return surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: key}, Author: author, Text: mention + " " + text,
		Call: true, OwnMentions: []surface.Span{{Start: 0, End: len(mention)}}}
}

func TestConversationCallsRouteAndDeduplicate(t *testing.T) {
	f := conversationsFront(t)
	for _, thread := range []string{"first", "second"} {
		conv := surface.Conversation{Channel: channel, Thread: thread}
		in := callMessage(conv, thread+"-call", fake.Person(owner), "hello "+thread)
		for i := 0; i < 2; i++ {
			if err := f.fe.handle(in); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := f.runner.all(); len(got) != 2 || got[0].Session == got[1].Session || got[0].Thread == got[1].Thread {
		t.Fatalf("launches: %+v", got)
	}
	for i, l := range f.runner.all() {
		name := []string{"first", "second"}[i]
		if l.Input.Text != "hello "+name {
			t.Fatalf("call text: %q", l.Input.Text)
		}
		if err := f.core.Machine.Apply(context.Background(), l.Session, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 1, Kind: agent.SessionReady, SessionID: name}}); err != nil {
			t.Fatal(err)
		}
		in := callMessage(surface.Conversation{Channel: channel, Thread: name}, name+"-next", fake.Person(owner), "next "+name)
		if err := f.fe.handle(in); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.runner.handed(), "; "); got != "deliver next first; deliver next second" {
		t.Fatalf("inputs: %s", got)
	}
}

func TestConversationCallInChannelOpensOnceOnMessage(t *testing.T) {
	f := conversationsFront(t)
	in := callMessage(chanConv, "channel-call", fake.Person(owner), "hello")
	for i := 0; i < 2; i++ {
		if err := f.fe.handle(in); err != nil {
			t.Fatal(err)
		}
	}
	got := f.threads()
	if len(got) != 1 || got[0].From != in.Ref.ID || len(f.runner.all()) != 1 {
		t.Fatalf("threads: %+v, sessions: %+v", got, f.runner.all())
	}
}

func TestConversationIgnoresUncalledAndUnauthorizedMessages(t *testing.T) {
	for _, who := range []surface.Author{fake.Person("guest"), fake.Bot(owner), fake.Webhook(hook)} {
		f := conversationsFront(t)
		in := callMessage(surface.Conversation{Channel: channel, Thread: "there"}, "call", who, "secret text")
		if who.Kind != surface.Person {
			in.Call, in.RejectedCall = false, true
		}
		if err := f.fe.handle(in); err != nil {
			t.Fatal(err)
		}
		if len(f.runner.all()) != 0 || len(f.threads()) != 0 {
			t.Fatal("unauthorized call started work")
		}
		if got := f.log.lines(); len(got) != 1 {
			t.Fatalf("log: %v", got)
		}
		var n int
		f.core.Store.DB.QueryRow(`SELECT count(*) FROM chat_triggers`).Scan(&n)
		if n != 0 {
			t.Fatal("stored refused call")
		}
	}
	f := conversationsFront(t)
	in := surface.Inbound{Ref: surface.MessageRef{Conv: surface.Conversation{Channel: channel, Thread: "there"}, ID: "plain"}, Author: fake.Person(owner), Text: "private words"}
	if err := f.fe.handle(in); err != nil {
		t.Fatal(err)
	}
	if len(f.log.lines()) != 0 || len(f.runner.all()) != 0 {
		t.Fatal("plain message was handled")
	}
	var n int
	f.core.Store.DB.QueryRow(`SELECT (SELECT count(*) FROM events)+(SELECT count(*) FROM chat_triggers)`).Scan(&n)
	if n != 0 {
		t.Fatal("plain message left stored text")
	}
}

func TestPrivateConversationRefusesCallsAndAttach(t *testing.T) {
	f := conversationsFront(t)
	conv := surface.Conversation{Channel: channel, Thread: "private"}
	f.driver.SetPrivate(conv, true)
	in := callMessage(conv, "private-call", fake.Person(owner), "hello")
	if err := f.fe.handle(in); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.all()) != 0 {
		t.Fatal("private conversation started")
	}
	waitFor(t, "private explanation", func() bool { return len(f.driver.Calls(conv)) > 0 })
	public := callMessage(surface.Conversation{Channel: channel, Thread: "public"}, "public-call", fake.Person(owner), "hello")
	if err := f.fe.handle(public); err != nil {
		t.Fatal(err)
	}
	l := f.runner.all()[0]
	if _, err := f.core.As(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpRead | control.OpSend}).Link(context.Background(), l.Thread, control.LinkRef{Client: "chat", Channel: channel, Thread: conv.Thread}); err == nil {
		t.Fatal("private Link allowed")
	}
}

func TestOpenedConversationMentionAndFailureRules(t *testing.T) {
	f := conversationsFront(t)
	first := callMessage(chanConv, "opened", fake.Person(owner), "hello")
	if err := f.fe.handle(first); err != nil {
		t.Fatal(err)
	}
	l := f.runner.all()[0]
	conv := f.threads()[0].Opened
	if err := f.core.Machine.Apply(context.Background(), l.Session, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 1, Kind: agent.SessionReady}}); err != nil {
		t.Fatal(err)
	}
	for i, others := range []bool{true, false} {
		in := callMessage(conv, []string{"others", "both"}[i], fake.Person(owner), "next")
		in.OthersOnly = others
		if err := f.fe.handle(in); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.runner.handed(); len(got) != 1 || got[0] != "deliver next" {
		t.Fatalf("inputs: %v", got)
	}
	// A plain message in a bot-opened conversation needs no mention.
	if err := f.fe.handle(surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: "plain-next"}, Author: fake.Person(owner), Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.handed(); len(got) != 2 {
		t.Fatalf("inputs: %v", got)
	}
}

func TestFailedConversationCallRetriesSameNumberOnce(t *testing.T) {
	f := conversationsFront(t)
	conv := surface.Conversation{Channel: channel, Thread: "existing"}
	first := callMessage(conv, "first", fake.Person(owner), "hello")
	if err := f.fe.handle(first); err != nil {
		t.Fatal(err)
	}
	old := f.runner.all()[0]
	if err := f.core.Machine.Apply(context.Background(), old.Session, session.Input{Kind: session.Exit, Reason: "start: could not confine"}); err != nil {
		t.Fatal(err)
	}
	plain := surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: "plain"}, Author: fake.Person(owner), Text: "plain"}
	if err := f.fe.handle(plain); err != nil {
		t.Fatal(err)
	}
	if len(f.driver.Calls(conv)) != 0 {
		t.Fatal("answered an uncalled message in an existing failed thread")
	}
	next := callMessage(conv, "retry", fake.Person(owner), "try again")
	for i := 0; i < 2; i++ {
		if err := f.fe.handle(next); err != nil {
			t.Fatal(err)
		}
	}
	got := f.runner.all()
	if len(got) != 2 || got[0].Thread != got[1].Thread || got[0].Session == got[1].Session || got[1].Input.Text != "try again" {
		t.Fatalf("launches: %+v", got)
	}
}

type privacyUnknown struct{ surface.Driver }

func TestUnknownPrivacyRefusesAndWorkerCallContinues(t *testing.T) {
	f := conversationsFront(t)
	f.fe.Driver = privacyUnknown{f.driver}
	in := callMessage(surface.Conversation{Channel: channel, Thread: "unknown"}, "unknown", fake.Person(owner), "hello")
	if err := f.fe.handle(in); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.all()) != 0 {
		t.Fatal("unknown privacy started a conversation")
	}
	id := f.runningThread(t)
	worker := callMessage(f.conv(id), "worker-call", fake.Person(owner), "hello worker")
	for i := 0; i < 2; i++ {
		if err := f.fe.handle(worker); err != nil {
			t.Fatal(err)
		}
	}
	got := f.runner.handed()
	if len(got) != 2 || !strings.Contains(got[1], "hello worker") {
		t.Fatalf("worker inputs: %v", got)
	}
}

func TestConversationOrderTakesPrecedence(t *testing.T) {
	for _, conv := range []surface.Conversation{chanConv, {Channel: channel, Thread: "existing"}} {
		f := conversationsFront(t)
		in := callMessage(conv, "order", fake.Person(owner), "!go repo:https://example.com/r.git do the work")
		if err := f.fe.handle(in); err != nil {
			t.Fatal(err)
		}
		got := f.runner.all()
		if len(got) != 1 || got[0].Confined {
			t.Fatalf("launches: %+v", got)
		}
	}
}

func TestClosedExistingConversationNeedsCallForClosedLine(t *testing.T) {
	f := conversationsFront(t)
	conv := surface.Conversation{Channel: channel, Thread: "existing"}
	if err := f.fe.handle(callMessage(conv, "first", fake.Person(owner), "hello")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE threads SET state = 'closed'`, `UPDATE sessions SET state = 'closed'`} {
		if _, err := f.core.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.fe.handle(surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: "plain"}, Author: fake.Person(owner), Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	if len(f.driver.Calls(conv)) != 0 {
		t.Fatal("plain message got a closed reply")
	}
	if err := f.fe.handle(callMessage(conv, "called", fake.Person(owner), "hello")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "closed reply", func() bool { return len(f.driver.Calls(conv)) > 0 })
}

func TestFailedConversationRetriesThroughSecondaryLink(t *testing.T) {
	f := conversationsFront(t)
	f.fe.ShowIn = map[string][]string{"c9": {channel}}
	first := callMessage(surface.Conversation{Channel: channel, Thread: "first"}, "first", fake.Person(owner), "hello")
	if err := f.fe.handle(first); err != nil {
		t.Fatal(err)
	}
	old := f.runner.all()[0]
	target := control.LinkRef{Client: "chat", Channel: "c9", Thread: "secondary"}
	if _, err := f.core.As(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpRead | control.OpSend}).Link(context.Background(), old.Thread, target); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Machine.Apply(context.Background(), old.Session, session.Input{Kind: session.Exit, Reason: "start: unavailable"}); err != nil {
		t.Fatal(err)
	}
	next := callMessage(surface.Conversation{Channel: target.Channel, Thread: target.Thread}, "retry-secondary", fake.Person(owner), "continue here")
	if err := f.fe.handle(next); err != nil {
		t.Fatal(err)
	}
	got := f.runner.all()
	if len(got) != 2 || got[1].Thread != old.Thread || got[1].Input.Link == nil || *got[1].Input.Link != target {
		t.Fatalf("launches: %+v", got)
	}
}

func TestPrivateConversationGuestGetsNoReply(t *testing.T) {
	f := conversationsFront(t)
	conv := surface.Conversation{Channel: channel, Thread: "private"}
	f.driver.SetPrivate(conv, true)
	if err := f.fe.handle(callMessage(conv, "guest-call", fake.Person("guest"), "secret")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.core.Store.DB.QueryRow(`SELECT (SELECT count(*) FROM events)+(SELECT count(*) FROM chat_triggers)+(SELECT count(*) FROM chat_notices)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(f.driver.Calls(conv)) != 0 || len(f.runner.all()) != 0 || len(f.log.lines()) != 1 {
		t.Fatalf("stored=%d calls=%v launches=%v logs=%v", n, f.driver.Calls(conv), f.runner.all(), f.log.lines())
	}
}

func TestExistingConversationIsNeverArchivedByLifecycle(t *testing.T) {
	f := conversationsFront(t)
	conv := surface.Conversation{Channel: channel, Thread: "existing"}
	if err := f.fe.handle(callMessage(conv, "call", fake.Person(owner), "hello")); err != nil {
		t.Fatal(err)
	}
	f.fe.ArchiveWhenAsleep = map[string]bool{channel: true}
	id := f.runner.all()[0].Thread
	for _, change := range []string{`{"from":"idle","to":"asleep"}`, `{"from":"asleep","to":"idle"}`, `{"from":"idle","to":"closed"}`} {
		rows, err := f.fe.archiveRows(context.Background(), control.Event{Thread: id, Scope: channel, Kind: "state", Data: []byte(change)}, []surface.Conversation{conv})
		if err != nil || len(rows) != 0 {
			t.Fatalf("lifecycle changed an existing conversation: %v, %v", rows, err)
		}
	}
}

func TestDisablingNewConversationsKeepsExistingInput(t *testing.T) {
	for _, disableScope := range []bool{false, true} {
		t.Run(fmt.Sprint("scope-disabled=", disableScope), func(t *testing.T) {
			f := conversationsFront(t)
			first := callMessage(chanConv, "first", fake.Person(owner), "hello")
			if err := f.fe.handle(first); err != nil {
				t.Fatal(err)
			}
			old := f.runner.all()[0]
			conv := f.threads()[0].Opened
			if err := f.core.Machine.Apply(context.Background(), old.Session, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 1, Kind: agent.SessionReady}}); err != nil {
				t.Fatal(err)
			}
			f.fe.Scopes[channel] = Scope{Enabled: !disableScope, Conversations: false}
			if disableScope {
				sc := f.core.Scopes[channel]
				sc.Enabled = false
				f.core.Scopes[channel] = sc
			}
			if err := f.fe.handle(surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: "plain"}, Author: fake.Person(owner), Text: "continue"}); err != nil {
				t.Fatal(err)
			}
			if got := f.runner.handed(); len(got) != 1 || got[0] != "deliver continue" {
				t.Fatalf("existing input: %v", got)
			}
			if err := f.fe.handle(callMessage(surface.Conversation{Channel: channel, Thread: "new"}, "new-call", fake.Person(owner), "new")); err != nil {
				t.Fatal(err)
			}
			if len(f.runner.all()) != 1 {
				t.Fatal("started a disabled new conversation")
			}
			var n int
			if err := f.core.Store.DB.QueryRow(`SELECT count(*) FROM chat_triggers WHERE message = 'new-call'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("stored disabled call: %d, %v", n, err)
			}

		})
	}
}
