package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The daemon's start, the waker and the chat front end find the
// supervisor's own thread, with a conversation older and one newer than
// it, open and closed (#867).
func TestConversationsBesideTheSupervisorsThread(t *testing.T) {
	wt := newWakeTest(t)
	wt.r.deadline = time.Now().Add(120 * time.Second)
	ctx := context.Background()
	var running atomic.Pointer[core.Core]
	prev := coreHook
	coreHook = func(c *core.Core) { prev(c); running.Store(c) }
	stop, owner, first := wt.start(t)
	restart := func() control.ThreadInfo {
		t.Helper()
		stop()
		var sv control.ThreadInfo
		stop, owner, sv = wt.start(t)
		return sv
	}
	defer func() { stop() }()
	var dir string
	if err := wt.db.QueryRow(`SELECT dir FROM supervisor_dirs WHERE scope = 'c1'`).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	talk := func(key string) control.ThreadID {
		t.Helper()
		id, err := running.Load().StartConfined(ctx, control.Caller{Principal: "boss", Scopes: []string{"c1"}, May: control.OpRead | control.OpStart},
			control.StartRequest{Key: key, Scope: "c1", Thread: "t-" + key, Input: control.Input{Text: "reply hello"}}, control.KindConversation, core.InDir(dir))
		if err != nil {
			t.Fatal(err)
		}
		wt.ended(t, id, 1)
		return id
	}
	closeThread := func(ids ...control.ThreadID) {
		t.Helper()
		for _, id := range ids {
			if err := owner.CloseThread(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	// wakes has a worker's turn end wake sv, and nothing else.
	n := 0
	wakes := func(sv control.ThreadID, want int, not ...control.ThreadID) {
		t.Helper()
		n++
		_, pos := wt.worker(t, owner, "c1", fmt.Sprintf("w%d", n), "reply done")
		wt.read(t, pos)
		wt.quiet(t)
		wt.woken(t, owner, sv, want)
		for _, id := range not {
			if got := wt.wakes(t, id); len(got) != 0 {
				t.Fatalf("thread %d was woken: %q", id, got)
			}
		}
	}

	older := talk("older") // newer than the first own thread, older than the next
	wakes(first.ID, 1, older)
	if sv := restart(); sv.ID != first.ID || supervisorThreads(t, owner, "c1") != 1 {
		t.Fatalf("a start beside conversation %d: the supervisor's thread is %d, want %d still", older, sv.ID, first.ID)
	}
	closeThread(first.ID)
	sv := restart()
	if sv.ID == first.ID || sv.ID == older {
		t.Fatalf("the closed supervisor %d was replaced by %d", first.ID, sv.ID)
	}
	wakes(sv.ID, 1, older)
	newer := talk("newer")
	wakes(sv.ID, 2, older, newer)
	if again := restart(); again.ID != sv.ID {
		t.Fatalf("a start beside conversations %d and %d: the supervisor's thread is %d, want %d still", older, newer, again.ID, sv.ID)
	}
	wakes(sv.ID, 3, older, newer)

	// The chat front end's line in the first one, closed, names it.
	wt.r.connected(t)
	conv := func(th control.ThreadInfo) surface.Conversation {
		if len(th.Links) != 1 {
			t.Fatalf("thread %d has the links %v", th.ID, th.Links)
		}
		return surface.Conversation{Channel: th.Links[0].Channel, Thread: th.Links[0].Thread}
	}
	in := wt.r.driver.Inject(conv(first), fakesurface.Webhook(e2eHook), "are you there")
	want := fmt.Sprintf("%s\nThe supervisor is now in thread %d: %s.", chat.ClosedLine, sv.ID, wt.r.driver.Link(conv(sv)))
	var got string
	eventually(t, "the closed line", wt.r.within(15*time.Second), func() bool {
		for _, c := range wt.r.driver.Calls(conv(first)) {
			if c.Op == fakesurface.OpPost && c.Message.ReplyTo != nil && c.Message.ReplyTo.ID == in.Ref.ID {
				got = c.Message.Text
			}
		}
		return got != ""
	})
	if got != want {
		t.Fatalf("in the closed supervisor's thread: %q, want %q", got, want)
	}

	closeThread(older, newer)
	if again := restart(); again.ID != sv.ID || supervisorThreads(t, owner, "c1") != 2 {
		t.Fatalf("a start beside closed conversations: the supervisor's thread is %d, want %d still", again.ID, sv.ID)
	}
	wakes(sv.ID, 4, older, newer)
}
