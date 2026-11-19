package chat

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// secondLink links thread id to one more conversation of this front end,
// as #228's attach will, and returns it.
func (f *front) secondLink(t *testing.T, id control.ThreadID, thread string) surface.Conversation {
	t.Helper()
	if _, err := f.fe.Outbox.Store.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, made_by, created_at)
		VALUES (?, ?, ?, ?, 'owner', 0)`, int64(id), f.fe.Name, channel, thread); err != nil {
		t.Fatal(err)
	}
	return surface.Conversation{Channel: channel, Thread: thread}
}

func posts(d *fake.Driver, conv surface.Conversation, text string) int {
	n := 0
	for _, c := range d.Calls(conv) {
		if c.Op == fake.OpPost && c.Message.Text == text {
			n++
		}
	}
	return n
}

// A message in a conversation with no link reaches no thread, also when a
// thread was once shown there.
func TestAConversationWithNoLinkReachesNothing(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	conv := f.conv(id)
	if _, err := f.fe.Outbox.Store.DB.Exec(`DELETE FROM links WHERE thread_id = ?`, int64(id)); err != nil {
		t.Fatal(err)
	}
	before := f.runner.handed()
	f.fe.Message(f.driver.Inject(conv, fake.Webhook(hook), "to nobody"))
	time.Sleep(100 * time.Millisecond)
	if got := f.runner.handed(); !slices.Equal(got, before) {
		t.Fatalf("a message with no link was handed over: %v, was %v", got, before)
	}
	var n int
	f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'input' AND json_extract(data, '$.text') = 'to nobody'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d input events of it", n)
	}
}

// A thread with two links: each conversation is caught up from its own
// cursor after a restart, every message reaches the thread once, and the
// agent's answer is posted once in each, also when the reader starts again.
func TestEachLinkIsCaughtUpFromItsCursorAndPostedOnce(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	first := f.conv(id)
	second := f.secondLink(t, id, "second-conv")

	stop := f.connect(t)
	f.driver.Inject(first, fake.Webhook(hook), "one")
	f.driver.Inject(second, fake.Webhook(hook), "uno")
	waitFor(t, "both live messages", func() bool { return len(f.runner.handed()) == 3 })
	stop() // the daemon goes

	two := f.driver.Inject(first, fake.Webhook(hook), "two")
	dos := f.driver.Inject(second, fake.Webhook(hook), "dos")
	f.connect(t) // and comes back
	waitFor(t, "the messages while down", func() bool { return len(f.runner.handed()) == 5 })
	f.fe.catchUp(context.Background(), first) // again: nothing new
	f.fe.catchUp(context.Background(), second)
	time.Sleep(100 * time.Millisecond)
	got := f.runner.handed()
	want := []string{"deliver first input", "deliver one", "deliver uno", "deliver two", "deliver dos"}
	if len(got) != len(want) || got[0] != want[0] || !slices.Contains(got, "deliver dos") || !slices.Contains(got, "deliver two") {
		t.Fatalf("handed over %v, want each of %v once", got, want)
	}
	for _, m := range []string{"deliver one", "deliver uno", "deliver two", "deliver dos"} {
		if n := strings.Count(strings.Join(got, ";")+";", m+";"); n != 1 {
			t.Fatalf("%s handed over %d times: %v", m, n, got)
		}
	}
	if c := f.storedCursor(first); c != two.Cursor {
		t.Fatalf("the first conversation's cursor %q, want %q", c, two.Cursor)
	}
	if c := f.storedCursor(second); c != dos.Cursor {
		t.Fatalf("the second conversation's cursor %q, want %q", c, dos.Cursor)
	}

	stopShow := f.showing(t)
	f.event(t, id, "text", map[string]any{"text": "the answer", "final": true})
	waitFor(t, "the answer in both", func() bool {
		return posts(f.driver, first, "the answer") == 1 && posts(f.driver, second, "the answer") == 1
	})
	// Each copy has a key of its own; the first link's is the key it
	// always had.
	var keys []string
	for _, conv := range []surface.Conversation{first, second} {
		for _, c := range f.driver.Calls(conv) {
			if c.Op == fake.OpPost && c.Message.Text == "the answer" {
				keys = append(keys, c.Key)
			}
		}
	}
	if len(keys) != 2 || keys[0] == keys[1] || strings.Contains(keys[0], "@") {
		t.Fatalf("the answer's keys %v, want two, the first without a link", keys)
	}
	stopShow()
	f.showing(t) // the reader starts again from its cursor
	time.Sleep(200 * time.Millisecond)
	if a, b := posts(f.driver, first, "the answer"), posts(f.driver, second, "the answer"); a != 1 || b != 1 {
		t.Fatalf("the answer posted %d and %d times", a, b)
	}
}

// The text form of attach is for a surface without commands, and a bot
// account's post attaches nothing.
func TestTextAttachOnlyWithoutCommandsAndNotFromABot(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	f.fe.ShowIn = map[string][]string{"c9": {channel}}
	text := "!go attach " + strconv.FormatInt(int64(id), 10)
	c9 := surface.Conversation{Channel: "c9"}
	count := func() int {
		var n int
		f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM links WHERE thread_id = ?`, int64(id)).Scan(&n)
		return n
	}
	f.fe.Message(f.driver.Inject(c9, fake.Bot(hook), text)) // a bot account with a person's id
	lim := fake.DefaultLimits
	lim.Commands = true
	f.driver.SetLimits(lim)
	f.fe.Message(f.driver.Inject(c9, fake.Webhook(hook), text))
	time.Sleep(100 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("%d links after a bot's attach and one on a surface with commands, want 1", n)
	}
	// The same post on a surface without commands attaches.
	f.driver.SetLimits(fake.DefaultLimits)
	f.fe.Message(f.driver.Inject(c9, fake.Webhook(hook), text))
	waitFor(t, "the text attach", func() bool { return count() == 2 })
}

// Only what came through a link is shown in the others: an input of no
// link, such as the supervisor's, is not.
func TestMirrorOnlyWhatCameThroughALink(t *testing.T) {
	f := newFront(t, workspaces{})
	convs := []surface.Conversation{{Channel: channel, Thread: "a"}, {Channel: channel, Thread: "b"}}
	through := control.Event{Pos: 9, Kind: "input", Data: []byte(`{"session":1,"turn":1,"text":"hi","principal":"grace","link":{"client":"chat","channel":"c1","thread":"a"}}`)}
	none := control.Event{Pos: 10, Kind: "input", Data: []byte(`{"session":1,"turn":1,"text":"[supervisor]\nhi","principal":"owner"}`)}
	if rows := f.fe.mirrorRows(through, convs); len(rows) != 1 || rows[0].Conv != convs[1] || rows[0].Message.Text != "grace: hi" {
		t.Fatalf("rows of an input through a link: %+v", rows)
	}
	if rows := f.fe.mirrorRows(none, convs); len(rows) != 0 {
		t.Fatalf("rows of an input through no link: %+v", rows)
	}
}
