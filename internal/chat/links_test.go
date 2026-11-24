package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
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

// busy is what the front end has done so far: the events stored, the
// messages handed over, its reader's cursor, and its outbox's rows by
// state. It changes while the front end works. Each read waits at most a
// second for the store, so that a front end stuck holding it still lets
// the wait fail.
func (f *front) busy() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var events, cursor, pending, sent int64
	db := f.fe.Outbox.Store.DB
	err := db.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM events`).Scan(&events)
	if err == nil {
		err = db.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM chat_cursors`).Scan(&cursor)
	}
	if err == nil {
		err = db.QueryRowContext(ctx, `SELECT count(CASE WHEN state = 'pending' THEN 1 END), count(CASE WHEN state = 'sent' THEN 1 END) FROM chat_outbox`).Scan(&pending, &sent)
	}
	if err != nil {
		return fmt.Sprintf("the store cannot be read: %v", err)
	}
	return fmt.Sprintf("events up to %d, %d handed over, cursor %d, %d rows pending, %d sent", events, len(f.runner.handed()), cursor, pending, sent)
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
	waitWhileBusy(t, "both live messages", f.busy, func() bool { return len(f.runner.handed()) == 3 })
	stop() // the daemon goes

	two := f.driver.Inject(first, fake.Webhook(hook), "two")
	dos := f.driver.Inject(second, fake.Webhook(hook), "dos")
	f.connect(t) // and comes back
	waitWhileBusy(t, "the messages while down", f.busy, func() bool { return len(f.runner.handed()) == 5 })
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
	// The reader, new, reads every event from the start before it reaches
	// the answer, and each makes rows to post: on a slow machine that takes
	// more than five seconds, but not five seconds with nothing done.
	waitWhileBusy(t, "the answer in both", f.busy, func() bool {
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

// Only what came through a link is shown in the others, and what came
// through no link but names its way (via) in every link; an input of
// neither, such as the supervisor's, is not (#605).
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
	for _, kind := range []control.EventKind{"input", "answer"} {
		dash := control.Event{Pos: 11, Kind: kind, Data: []byte(`{"session":1,"turn":1,"text":"from the page","principal":"ada","via":"dashboard"}`)}
		rows := f.fe.mirrorRows(dash, convs)
		if len(rows) != 2 || rows[0].Conv != convs[0] || rows[1].Conv != convs[1] {
			t.Fatalf("rows of an %s through the dashboard: %+v, want one in each link", kind, rows)
		}
		for _, r := range rows {
			if r.Message.Text != "ada: from the page" || !r.Message.Quiet {
				t.Fatalf("a row of an %s through the dashboard: %+v, want a quiet \"ada: from the page\"", kind, r)
			}
		}
	}
}

// shownIn attaches thread id in channel ch and returns the chat thread it
// is shown in there.
func (f *front) shownIn(t *testing.T, id control.ThreadID, ch string) surface.Conversation {
	t.Helper()
	f.command(surface.Conversation{Channel: ch}, fake.Person(owner), "attach-"+ch, attachCommand,
		surface.Arg{Name: "number", Value: strconv.FormatInt(int64(id), 10)})
	for _, c := range f.driver.Calls(surface.Conversation{Channel: ch}) {
		if c.Op == fake.OpOpenThread {
			return c.Opened
		}
	}
	t.Fatalf("the attach opened no thread in %s", ch)
	return surface.Conversation{}
}

// A channel taken out of a scope's show_in gets no more of the scope's
// threads: a link made while it was listed posts nothing there once the
// daemon runs with the new config, and what is written there is no input,
// then or later. With the entry back, the thread is shown there again
// (#572).
func TestNothingIsPostedInAChannelRemovedFromShowIn(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.ShowIn = map[string][]string{"c9": {channel}}
	})
	id := f.runningThread(t)
	th := f.conv(id)
	shown := f.shownIn(t, id, "c9")
	ctx := context.Background()
	start := func(meanwhile ...string) { // as Run and Connected
		t.Helper()
		time.Sleep(5 * time.Millisecond) // later than the last message, in milliseconds
		f.fe.in, f.fe.inboundOnce = nil, sync.Once{}
		f.fe.inboundState().started()
		for _, text := range meanwhile { // the service could not be reached yet
			f.driver.Inject(shown, fake.Person(owner), text)
			time.Sleep(5 * time.Millisecond)
		}
		if err := f.fe.settleLinks(ctx); err != nil {
			t.Fatal(err)
		}
		convs, err := f.fe.toCatchUp(ctx)
		if err != nil || !slices.Contains(convs, th) {
			t.Fatalf("to catch up: %v, %v", convs, err)
		}
		if got := slices.Contains(convs, shown); got != (f.fe.ShowIn["c9"][0] == channel) {
			t.Fatalf("%v caught up: %v", shown, got)
		}
		for _, c := range convs {
			f.fe.catchUp(ctx, c)
		}
	}
	// The operator removes c9 from the scope's show_in and restarts. c9 is
	// listed for another scope, whose threads are still shown there.
	f.fe.ShowIn = map[string][]string{"c9": {"off"}}
	f.core.Scopes[channel] = core.Scope{Enabled: true, Owner: "chat"}
	before := f.runner.handed()
	start()
	f.deliver(shown, fake.Person(owner), "from the removed channel")
	if got := f.runner.handed(); !slices.Equal(got, before) {
		t.Fatalf("input from the removed channel reached the agent: %q", got)
	}
	// Written last, while the front end was not connected.
	f.driver.Inject(shown, fake.Person(owner), "missed, from the removed channel")
	start()
	stop := f.showing(t)
	f.event(t, id, "input", map[string]any{"text": "typed elsewhere", "principal": "owner", "via": "web", "turn": 9})
	f.event(t, id, "text", map[string]any{"text": "secret answer", "final": true})
	waitFor(t, "the answer in the scope's own channel", func() bool { return posts(f.driver, th, "secret answer") == 1 })
	f.settled(t, th, shown)
	for _, text := range []string{"secret answer", "owner: typed elsewhere"} {
		if n := posts(f.driver, shown, text); n != 0 {
			t.Errorf("%q was posted %d time(s) in a channel no longer in show_in: %q", text, n, postsIn(f.driver, shown))
		}
	}
	stop()
	f.fe.ShowIn = map[string][]string{"c9": {channel}}
	f.core.Scopes[channel] = core.Scope{Enabled: true, Owner: "chat", ShowIn: []string{"chat:c9"}}
	// From this start on it is input, also after one more.
	start("while starting")
	f.driver.Inject(shown, fake.Person(owner), "missed, after the entry is back")
	start()
	got := strings.Join(f.runner.handed(), "\n")
	for text, want := range map[string]int{"from the removed channel": 0, "while starting": 1, "missed, after the entry is back": 1} {
		if n := strings.Count(got, text); n != want {
			t.Errorf("%q became input %d time(s), want %d: %q", text, n, want, got)
		}
	}
	f.showing(t)
	f.event(t, id, "text", map[string]any{"text": "after the entry is back", "final": true})
	waitFor(t, "the answer in the channel listed again", func() bool { return posts(f.driver, shown, "after the entry is back") == 1 })
}

// What was still to be posted in a channel when it left show_in is not
// posted there, and one log line at start names the link; the rows of the
// other conversations are posted (#572).
func TestPendingRowsOfARemovedChannelAreDropped(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.ShowIn = map[string][]string{"c9": {channel}, "c8": {channel}}
	})
	f.core.Scopes[channel] = core.Scope{Enabled: true, Owner: "chat", ShowIn: []string{"chat:c9", "chat:c8"}}
	id := f.runningThread(t)
	th, c9, c8 := f.conv(id), f.shownIn(t, id, "c9"), f.shownIn(t, id, "c8")
	f.settled(t, th, c9, c8)
	// Another client's scope has the key c7, which is a channel of this one
	// too: the channel is not the scope's own.
	c7 := surface.Conversation{Channel: "c7", Thread: "t7"}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (900, 'c7', 'k', 'owner', 'closed', 0, 0)`,
		`INSERT INTO links (thread_id, client, channel, thread, created_at) VALUES (900, 'chat', 'c7', 't7', 0)`,
	} {
		if _, err := f.fe.Outbox.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.driver.Fail(fake.OpPost, 1<<30, errors.New("network is unreachable"))
	ctx, cancel := context.WithCancel(context.Background())
	for i, conv := range []surface.Conversation{th, c9, c8, c7} {
		if err := f.fe.Outbox.Write(ctx, row(uint64(1<<41+i), 0, conv, "secret answer")); err != nil {
			t.Fatal(err)
		}
	}

	// The daemon runs again, with c8 alone listed.
	d, log := fake.New(), &logBuf{}
	lg := slog.New(slog.NewTextHandler(log, nil))
	fe := &FrontEnd{Name: "chat", Driver: d, API: f.fe.API, Trigger: "!go", People: testPeople(), Log: lg,
		Outbox: &Outbox{Store: f.fe.Outbox.Store, Client: "chat", Driver: d, Log: lg, Retry: 5 * time.Millisecond},
		Scopes: f.fe.Scopes, ShowIn: map[string][]string{"c8": {channel}}}
	done := make(chan error, 1)
	go func() { done <- fe.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for _, conv := range []surface.Conversation{th, c8} {
		waitFor(t, "the row of a served conversation", func() bool { return posts(d, conv, "secret answer") == 1 })
	}
	for i, conv := range map[int]surface.Conversation{1: c9, 3: c7} {
		var state string
		if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT state FROM chat_outbox WHERE event = ?`, 1<<41+i).Scan(&state); err != nil || state != "dropped" {
			t.Errorf("the row of %v is %q, %v", conv, state, err)
		}
		if n := len(postsIn(d, conv)); n != 0 {
			t.Errorf("%d posts in %v", n, conv)
		}
	}
	var lines []string
	for _, l := range log.lines() {
		if strings.Contains(l, "links are no longer served") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], c9.Channel+"/"+c9.Thread) || strings.Contains(lines[0], c8.Channel+"/"+c8.Thread) {
		t.Errorf("the log's lines about the link: %q", lines)
	}
}
