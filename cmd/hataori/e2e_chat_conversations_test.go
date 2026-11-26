package main

import (
	"context"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// Calls reach independent real session hosts running the fake agent over
// pipes. The fake service checks that each completed answer returns to its
// own chat thread and that repeated delivery starts no second session.
func TestChatCallsStartAndContinueIndependentConversations(t *testing.T) {
	frontend := quickLines(t)
	f := newSvTalks(t, "")
	for _, name := range []string{"one", "two"} {
		conv := surface.Conversation{Channel: "c1", Thread: "chat-" + name}
		in := f.wt.r.driver.InjectWith(conv, fakesurface.Person(svCaller), "reply hello "+name, func(in *surface.Inbound) { in.Call = true })
		var id control.ThreadID
		eventually(t, "a conversation linked to "+name, f.wt.r.within(15*time.Second), func() bool {
			ts, err := f.owner.Threads(context.Background(), control.Filter{Link: &control.LinkRef{Client: fakeSurface, Channel: conv.Channel, Thread: conv.Thread}})
			if err != nil || len(ts) != 1 {
				return false
			}
			id = ts[0].ID
			return true
		})
		f.wt.ended(t, id, 1)
		f.wt.r.postedWith(t, conv, "hello "+name)
		// Re-deliver the exact inbound through the same front end.
		frontend().Message(in)
		f.wt.r.driver.InjectWith(conv, fakesurface.Person(svCaller), "reply next "+name, func(in *surface.Inbound) { in.Call = true })
		f.wt.ended(t, id, 2)
		f.wt.r.postedWith(t, conv, "next "+name)
		var sessions int
		if err := f.wt.db.QueryRow(`SELECT count(*) FROM sessions WHERE thread_id = ?`, int64(id)).Scan(&sessions); err != nil || sessions != 1 {
			t.Fatalf("sessions: %d, %v", sessions, err)
		}
		if events := threadEvents(t, f.owner, id, "input"); len(events) != 2 {
			t.Fatalf("inputs: %v", events)
		}
	}
}
