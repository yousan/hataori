package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// clientOwner opens conversations of the client name and counts them.
type clientOwner struct {
	name string
	n    int
}

func (o *clientOwner) NewThread(_ context.Context, channel, _, _ string) (control.LinkRef, error) {
	o.n++
	return control.LinkRef{Client: o.name, Channel: channel, Thread: fmt.Sprint("opened-", o.n)}, nil
}

func links(t *testing.T, f *fixture, id control.ThreadID) []control.LinkRef {
	t.Helper()
	ts, err := f.owner.Threads(context.Background(), control.Filter{Thread: id})
	if err != nil || len(ts) != 1 {
		t.Fatalf("thread %d: %v, %v", id, ts, err)
	}
	var out []control.LinkRef
	for _, l := range ts[0].Links {
		out = append(out, l.LinkRef)
	}
	return out
}

// A link is made in the scope's own channel or in one its show_in names,
// once per conversation; a conversation of another thread, and a channel
// not in show_in, are refused, make nothing and write one log line.
func TestLinkOnlyWhereShowInAllows(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	owner := &clientOwner{name: "two"}
	f.core.Owners["two"] = owner
	sc := f.core.Scopes["s"]
	sc.ShowIn = []string{"two:d1"}
	f.core.Scopes["s"] = sc
	a, _ := f.running(t, "t1")
	b, _ := f.running(t, "t2")
	opened := owner.n

	// An existing conversation in the scope's own channel, twice.
	l := control.LinkRef{Client: "chat", Channel: "s", Thread: "c-extra"}
	for range 2 {
		if got, err := f.owner.Link(ctx, a, l); err != nil || got != l {
			t.Fatalf("Link %v: %v, %v", l, got, err)
		}
	}
	if n := len(links(t, f, a)); n != 2 {
		t.Fatalf("%d links after linking one conversation twice, want 2", n)
	}

	// A conversation another thread shows.
	if _, err := f.owner.Link(ctx, b, l); !errors.Is(err, control.ErrLinkedElsewhere) {
		t.Fatalf("Link of thread %d's conversation to %d: %v", a, b, err)
	}

	// A new conversation in a channel show_in names, opened once.
	got, err := f.owner.Link(ctx, a, control.LinkRef{Client: "two", Channel: "d1"})
	if err != nil || got.Client != "two" || got.Channel != "d1" || got.Thread == "" || owner.n != opened+1 {
		t.Fatalf("Link in two:d1: %v, %v; opened %d", got, err, owner.n-opened)
	}
	again, err := f.owner.Link(ctx, a, control.LinkRef{Client: "two", Channel: "d1"})
	if err != nil || again != got || owner.n != opened+1 {
		t.Fatalf("Link in two:d1 again: %v, %v; opened %d", again, err, owner.n-opened)
	}

	// A channel not in show_in: nothing is made or opened, one line.
	before := len(links(t, f, a))
	f.log.Reset()
	for _, l := range []control.LinkRef{{Client: "two", Channel: "d2"}, {Client: "chat", Channel: "d1"}, {Client: "two", Channel: "s", Thread: "x"}} {
		if _, err := f.owner.Link(ctx, a, l); !errors.Is(err, ErrForbidden) {
			t.Fatalf("Link in %s:%s: %v, want refused", l.Client, l.Channel, err)
		}
	}
	if got := f.log.String(); strings.Count(got, "link refused") != 3 || !strings.Contains(got, "client=two channel=d2") ||
		!strings.Contains(got, "thread=1") {
		t.Fatalf("logged %q", got)
	}
	// A caller that may not see the thread's scope is answered as for a
	// thread that does not exist (#479), and the line names the thread and
	// its scope.
	f.log.Reset()
	stranger := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"elsewhere"}, May: all})
	if _, err := stranger.Link(ctx, a, control.LinkRef{Client: "two", Channel: "d2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Link by a caller of another scope: %v", err)
	}
	if got := f.log.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "op=Link") || !strings.Contains(got, "thread=1 thread_scope=s") {
		t.Fatalf("logged %q", got)
	}
	if n := len(links(t, f, a)); n != before || owner.n != opened+1 {
		t.Fatalf("refused links made %d links and opened %d conversations", n-before, owner.n-opened-1)
	}
}

// An input names only a link of its thread and of the client it came
// from: another thread's link, a conversation that is no link, a link of
// the same thread on another client, and any link from a caller with no
// client are left out of the input event.
func TestInputNamesOnlyALinkOfItsThread(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	f.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:1"}}}, "owner", nil)
	a, _ := f.running(t, "t1")
	b, _ := f.running(t, "t2")
	own := links(t, f, a)[0]
	other := links(t, f, b)[0]
	elsewhere := control.LinkRef{Client: "two", Channel: "d1", Thread: "x1"}
	if _, err := f.core.Store.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, created_at) VALUES (?, 'two', 'd1', 'x1', 0)`, int64(a)); err != nil {
		t.Fatal(err)
	}
	chat := f.core.As(control.Caller{From: "chat:1", Scopes: []string{"s"}, May: all})
	for i, c := range []struct {
		api control.API
		l   control.LinkRef
	}{{chat, own}, {chat, other}, {chat, control.LinkRef{Client: "chat", Channel: "s", Thread: "nowhere"}}, {chat, elsewhere}, {f.owner, own}} {
		l := c.l
		if err := c.api.Send(ctx, a, fmt.Sprint("m", i), control.Input{Text: fmt.Sprint("text ", i), Link: &l}, control.Deliver); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.core.Store.DB.Query(`SELECT json_extract(data, '$.text'), coalesce(json_extract(data, '$.link.thread'), '') FROM events
		WHERE thread_id = ? AND kind = 'input' AND json_extract(data, '$.text') LIKE 'text %' ORDER BY pos`, int64(a))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var text, link string
		rows.Scan(&text, &link)
		got = append(got, text+"="+link)
	}
	if want := "text 0=" + own.Thread + ",text 1=,text 2=,text 3=,text 4="; strings.Join(got, ",") != want {
		t.Fatalf("input events %v, want %s", got, want)
	}
}

// A thread's first input came through its first link only when the start
// came from that link's client.
func TestFirstInputLinkOnlyFromItsClient(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	f.core.People = people.New([]people.Person{{Name: "owner", IDs: []string{"chat:1"}}}, "owner", nil)
	chat := f.core.As(control.Caller{From: "chat:1", Scopes: []string{"s"}, May: all})
	for _, c := range []struct {
		api    control.API
		thread string
	}{{chat, "from-chat"}, {f.owner, "from-socket"}} {
		if _, err := c.api.Start(ctx, control.StartRequest{Key: c.thread, Scope: "s", Thread: c.thread, Input: control.Input{Text: "go"}}); err != nil {
			t.Fatal(err)
		}
	}
	var chatLink, socketLink string
	f.core.Store.DB.QueryRow(`SELECT coalesce(json_extract(first_input, '$.link.thread'), '') FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE t.key = 'from-chat'`).Scan(&chatLink)
	f.core.Store.DB.QueryRow(`SELECT coalesce(json_extract(first_input, '$.link.thread'), '') FROM sessions s JOIN threads t ON t.id = s.thread_id
		WHERE t.key = 'from-socket'`).Scan(&socketLink)
	if chatLink != "from-chat" || socketLink != "" {
		t.Fatalf("first inputs' links %q and %q, want the chat start's only", chatLink, socketLink)
	}
}

// A conversation the client opened for the thread is input from its start,
// and its link says that the client opened it. One that was there before,
// which a Link or a Start names, is input from when the link was made, and
// its link says nothing (#870).
func TestLinkToAConversationThatWasThere(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "opened"})
	f.core.Owners["two"] = &clientOwner{name: "two"}
	sc := f.core.Scopes["s"]
	sc.ShowIn = []string{"two:d1"}
	f.core.Scopes["s"] = sc
	named, err := f.owner.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "named", Repo: "r", Input: control.Input{Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Start(ctx, control.StartRequest{Key: "k2", Scope: "s", Repo: "r", Input: control.Input{Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	// A second Link there returns the link that is there, as it was made.
	for _, l := range []control.LinkRef{{Client: "chat", Channel: "s", Thread: "attached"}, {Client: "two", Channel: "d1"},
		{Client: "two", Channel: "d1"}, {Client: "two", Channel: "d1", Thread: "opened-1"}} {
		if _, err := f.owner.Link(ctx, named, l); err != nil {
			t.Fatal(err)
		}
	}
	told := map[string]bool{}
	ts, err := f.owner.Threads(ctx, control.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range ts {
		d, err := f.owner.Thread(ctx, th.ID, 0, 0)
		if err != nil || !slices.Equal(d.Links, th.Links) {
			t.Errorf("thread %d: links %+v, %v, in the list %+v", th.ID, d.Links, err, th.Links)
		}
		for _, l := range th.Links {
			told[l.Thread] = l.Opened
		}
	}
	if want := map[string]bool{"named": false, "opened": true, "attached": false, "opened-1": true}; !maps.Equal(told, want) {
		t.Errorf("the links say who opened their conversations: %v, want %v", told, want)
	}
	for thread, there := range map[string]bool{"named": true, "opened": false, "attached": true, "opened-1": false} {
		var made, since int64
		if err := f.core.Store.DB.QueryRow(`SELECT created_at, since FROM links WHERE thread = ?`, thread).Scan(&made, &since); err != nil {
			t.Fatalf("link to %s: %v", thread, err)
		}
		if want := made; there && since != want || !there && since != 0 {
			t.Errorf("link to %s: since %d, made %d, want it to count from when it was made: %v", thread, since, made, there)
		}
	}
}

func (*clientOwner) CheckLink(context.Context, control.LinkRef, control.ThreadKind) error { return nil }
