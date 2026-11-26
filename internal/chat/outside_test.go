package chat

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

func TestOutsidePostMentionsOnlyTheLastPerson(t *testing.T) {
	ctx := context.Background()
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindSupervisor)
	if err := f.core.Machine.Apply(ctx, 1, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.SessionReady, Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	api := f.core.As(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpSend | control.OpAnswer})
	if err := api.Send(ctx, id, "person", control.Input{Text: "hello", Outside: []control.OutsideText{{Kind: "message", ID: "outside"}}}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	post := func(want []string) {
		t.Helper()
		text := "@owner @hook"
		pos := f.event(t, id, "text", map[string]any{"text": text, "final": true})
		got, _, err := f.fe.svPost(ctx, &reader{}, control.Event{Thread: id, Pos: pos}, svThread{sv: true, scope: channel}, false, text)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("mentions %v, %v; want %v", got, err, want)
		}
	}
	post([]string{owner})
	// A person's answer during a wake turn must count too; a wake's own
	// daemon-written input must not replace the last person.
	if _, err := f.core.Store.DB.Exec(`UPDATE turns SET trigger = 'hataori-wake-first'`); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Machine.Apply(ctx, 1, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.Request, Seq: 2, Request: &agent.RequestInfo{ID: "q", Text: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	other := f.core.As(control.Caller{Principal: "hook", Scopes: []string{channel}, May: control.OpAnswer})
	if err := other.Answer(ctx, session.RequestHandle(1, "q"), control.Answer{Text: "this"}); err != nil {
		t.Fatal(err)
	}
	post([]string{hook})
}

func TestOutsideRequestsShowProvenance(t *testing.T) {
	line := store.OutsideLine([]control.OutsideText{{Kind: "message", ID: "source-message"}})
	text := proposalText(eventData{Number: 3, Action: "send", Target: 4, Text: "do it", Outside: []control.OutsideText{{Kind: "message", ID: "source-message"}}})
	if !strings.HasPrefix(text, line+"\n") || !strings.Contains(text, "do it") {
		t.Fatalf("proposal omitted mark or text: %s", text)
	}
}

func TestOutsidePendingPostAndQuietMentionsAreRechecked(t *testing.T) {
	ctx := context.Background()
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindSupervisor)
	if err := f.core.Machine.Apply(ctx, 1, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.SessionReady, Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	api := f.core.As(control.Caller{Principal: "owner", Scopes: []string{channel}, May: control.OpSend})
	if err := api.Send(ctx, id, "person", control.Input{Text: "hello"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	pos := f.event(t, id, "text", map[string]any{"text": "@owner @hook", "final": true})
	encoded, err := encode(Row{Event: pos, Message: surface.Message{Text: "@owner @hook", Notify: []string{owner, hook}}})
	if err != nil {
		t.Fatal(err)
	}
	// Both the serialized post and quiet-hour records were made before
	// outside text arrived during this same still-open turn.
	if err := f.core.Store.Update(ctx, func(tx *store.Tx) error {
		for _, name := range []string{"owner", "hook"} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_named (client, thread, end_at, name, times, pos) VALUES ('chat', ?, 1, ?, 1, ?)`, int64(id), name, pos); err != nil {
				return err
			}
		}
		return store.MarkConversation(ctx, tx, id, "", []control.OutsideText{{Kind: "message", ID: "late-outside"}})
	}); err != nil {
		t.Fatal(err)
	}
	driver := fake.New()
	o := &Outbox{Store: f.core.Store, Client: "chat", Driver: driver, People: testPeople(), Log: f.fe.Log}
	conv := surface.Conversation{Channel: channel, Thread: "replayed"}
	if got := o.send(ctx, conv, pending{event: pos, message: encoded}); got != posted {
		t.Fatalf("replay outcome: %v", got)
	}
	calls := driver.Calls(conv)
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Message.Notify, []string{owner}) {
		t.Fatalf("stale serialized mentions replayed: %+v", calls)
	}
	limited, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := f.core.Store.Update(limited, func(tx *store.Tx) error {
		ends, err := f.fe.dueQuietEnds(limited, tx, 2)
		if err != nil {
			return err
		}
		if len(ends) != 1 || !reflect.DeepEqual(ends[0].notify, []string{owner}) {
			t.Fatalf("stale quiet-hour mentions replayed: %+v", ends)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
