package chat

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// attachTo has the owner use /attach for thread id in conv.
func (f *front) attachTo(t *testing.T, id control.ThreadID, conv surface.Conversation) {
	t.Helper()
	time.Sleep(5 * time.Millisecond) // what was written so far is older than the link
	f.command(conv, fake.Person(owner), "attach-"+conv.Thread, attachCommand, surface.Arg{Name: "number", Value: strconv.FormatInt(int64(id), 10)})
	var n int
	f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM links WHERE thread_id = ? AND thread = ?`, int64(id), conv.Thread).Scan(&n)
	if n != 1 {
		t.Fatalf("the attach made %d links to the chat thread, want 1", n)
	}
	time.Sleep(5 * time.Millisecond) // and what is written from now on is newer
}

// caughtUp waits until conv is no longer behind.
func (f *front) caughtUp(t *testing.T, conv surface.Conversation) {
	t.Helper()
	waitWhileBusy(t, "the catch-up of the conversation", f.busy, func() bool { return !f.fe.inboundState().isBehind(conv) })
}

// What people wrote in a chat thread before a hataori thread was attached
// to it is no input, after a reconnect or a restart, whether the front end
// saw some of it live or nothing. What they write after the attach while
// nobody is connected is, once.
func TestMessagesFromBeforeTheAttachAreNotInput(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run("seen live "+strconv.FormatBool(live), func(t *testing.T) {
			f := newFront(t, workspaces{})
			id := f.runningThread(t)
			old := surface.Conversation{Channel: channel, Thread: "old-discussion"}
			if live { // which stores a cursor, with no link
				stop := f.connect(t)
				f.driver.Inject(old, fake.Person(owner), "shall we?")
				stop()
				if f.storedCursor(old) == "" {
					t.Fatal("the live message stored no cursor")
				}
			}
			// Nobody is connected: kept by the service, never delivered.
			f.driver.Inject(old, fake.Webhook(hook), "delete the release branch")
			last := f.driver.Inject(old, fake.Person(owner), "no, keep it")
			stop := f.connect(t)
			f.attachTo(t, id, old)

			before := f.runner.handed()
			f.driver.Disconnect()
			f.driver.Reconnect()
			f.caughtUp(t, old)
			if got := f.runner.handed(); !slices.Equal(got, before) {
				t.Fatalf("after a reconnect, messages from before the attach reached the agent:\n got  %q\n was  %q", got, before)
			}
			if got := f.storedCursor(old); got != last.Cursor {
				t.Fatalf("the cursor is %q, want %q: the messages from before are fetched at every catch-up", got, last.Cursor)
			}
			stop() // the daemon goes

			f.driver.Inject(old, fake.Person(owner), "after the attach")
			f.connect(t) // and comes back
			want := append(slices.Clone(before), "deliver after the attach")
			waitWhileBusy(t, "the message written while down", f.busy, func() bool { return len(f.runner.handed()) >= len(want) })
			f.caughtUp(t, old)
			f.fe.catchUp(context.Background(), old) // again: nothing new
			if got := f.runner.handed(); !slices.Equal(got, want) {
				t.Fatalf("handed over %q, want %q", got, want)
			}
		})
	}
}

// The first catch-up of an attached chat thread may come after the daemon
// was down: it has messages from before the attach and from after it.
func TestFirstCatchUpOfAnAttachedThreadStartsAtTheAttach(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	old := surface.Conversation{Channel: channel, Thread: "old-discussion"}
	f.driver.Inject(old, fake.Person(owner), "before")
	f.attachTo(t, id, old)
	f.driver.Inject(old, fake.Person(owner), "after")
	f.connect(t)
	want := []string{"deliver first input", "deliver after"}
	waitWhileBusy(t, "the message from after the attach", f.busy, func() bool { return len(f.runner.handed()) >= len(want) })
	f.caughtUp(t, old)
	if got := f.runner.handed(); !slices.Equal(got, want) {
		t.Fatalf("handed over %q, want %q", got, want)
	}
}

// Words that give a command on a surface without commands are no command
// either, when they were written before the attach.
func TestCommandInWordsFromBeforeTheAttachDoesNothing(t *testing.T) {
	f := newFront(t, workspaces{})
	f.driver.SetLimits(surface.Limits{})
	id := f.runningThread(t)
	old := surface.Conversation{Channel: channel, Thread: "old-discussion"}
	f.driver.Inject(old, fake.Person(owner), "!go stop")
	f.attachTo(t, id, old)
	before := f.runner.handed()
	f.connect(t)
	f.caughtUp(t, old)
	if got := f.runner.handed(); !slices.Equal(got, before) {
		t.Fatalf("handed over %q, was %q", got, before)
	}
	var n int
	f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM chat_notices WHERE message LIKE 'text:%'`).Scan(&n)
	if n != 0 {
		t.Fatal("the words from before the attach were taken as a command")
	}
}

// sinceThen is a driver that runs then after each Since, before the caller
// has the messages.
type sinceThen struct {
	surface.Driver
	then func()
}

func (d sinceThen) Since(ctx context.Context, conv surface.Conversation, cursor string) ([]surface.Inbound, error) {
	msgs, err := d.Driver.Since(ctx, conv, cursor)
	d.then()
	return msgs, err
}

// A chat thread with no link is caught up too while the open threads could
// not be listed. An attach that comes between the fetch of its messages and
// their handling does not make them input.
func TestAttachWhileTheChatThreadIsCaughtUp(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	old := surface.Conversation{Channel: channel, Thread: "old-discussion"}
	f.driver.Inject(old, fake.Person(owner), "before")
	f.fe.Driver = sinceThen{f.driver, func() { f.attachTo(t, id, old) }}
	before := f.runner.handed()
	f.fe.catchUp(context.Background(), old)
	if got := f.runner.handed(); !slices.Equal(got, before) {
		t.Fatalf("handed over %q, was %q", got, before)
	}
}

// Nor does one that comes after the message was found to have no link.
func TestAttachWhileAMessageIsHandledByTheCatchUp(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	old := surface.Conversation{Channel: channel, Thread: "old-discussion"}
	f.driver.Inject(old, fake.Person(owner), "before")
	as := f.fe.API
	f.fe.API = func(c control.Caller) control.API { // what handling a follow-up asks first
		f.fe.Outbox.Store.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, made_by, created_at, since)
			VALUES (?, 'chat', ?, ?, 'owner', ?1, ?1) ON CONFLICT DO NOTHING`, int64(id), old.Channel, old.Thread)
		return as(c)
	}
	before := f.runner.handed()
	f.fe.catchUp(context.Background(), old)
	if got := f.runner.handed(); !slices.Equal(got, before) {
		t.Fatalf("handed over %q, was %q", got, before)
	}
}
