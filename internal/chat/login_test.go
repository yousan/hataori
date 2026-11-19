package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// fakeRelay records what the front end asks of the relay. It runs no
// login command.
type fakeRelay struct {
	mu     sync.Mutex
	starts []string
	codes  []string
}

func (r *fakeRelay) Start(_ context.Context, caller, agent string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, string(caller)+" "+agent)
	return nil
}

func (r *fakeRelay) Code(caller, agent, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, string(caller)+" "+agent+" "+code)
	return nil
}

func (r *fakeRelay) got() (starts, codes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.starts...), append([]string(nil), r.codes...)
}

const link = "https://login.example.com/oauth?state=SECRETLINK"

func withRelay(r *fakeRelay) func(*FrontEnd) {
	return func(fe *FrontEnd) {
		fe.Instance = "test-instance"
		if r != nil {
			fe.Relay = r
		}
	}
}

// running runs the event reader and the login messages until stop.
func (f *front) running(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.fe.show(ctx) }()
	go func() { defer wg.Done(); f.fe.runLogins(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); wg.Wait() }) }
	t.Cleanup(stop)
	return stop
}

// raw appends an event with data as given.
func (f *front) raw(t *testing.T, id control.ThreadID, kind string, data map[string]any) {
	t.Helper()
	b, _ := json.Marshal(data)
	err := f.fe.Outbox.Store.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Append(context.Background(), control.Event{Thread: id, Kind: control.EventKind(kind), Data: b})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// bareThread makes a thread in the channel, linked to its conversation
// key, with no session.
func (f *front) bareThread(t *testing.T, key string) control.ThreadID {
	t.Helper()
	db := f.fe.Outbox.Store.DB
	res, err := db.Exec(`INSERT INTO threads (scope, key, started_by, state, created_at, updated_at) VALUES (?, ?, 'owner', 'idle', 0, 0)`, channel, key)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO links (thread_id, client, channel, thread, created_at) VALUES (?, ?, ?, ?, 0)`, id, f.fe.Name, channel, key); err != nil {
		t.Fatal(err)
	}
	return control.ThreadID(id)
}

func (f *front) lost(t *testing.T, id control.ThreadID, outage int) {
	t.Helper()
	f.event(t, id, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "failure", "reason": "login", "background": 0})
	f.raw(t, id, "login_lost", map[string]any{"agent": "agent", "outage": outage, "thread": 1})
}

func (f *front) stopped(t *testing.T, id control.ThreadID) {
	t.Helper()
	f.event(t, id, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "failure", "reason": "login", "background": 0})
}

func (f *front) dm() surface.Conversation {
	c, _ := f.driver.OpenDirect(context.Background(), owner)
	return c
}

// dmPosts returns the posts in the owner's direct conversation, and what
// the first one shows now.
func (f *front) dmPosts() (posts []fake.Call, first surface.Message) {
	for _, c := range f.driver.Calls(f.dm()) {
		switch {
		case c.Op == fake.OpPost:
			posts = append(posts, c)
			if len(posts) == 1 {
				first = c.Message
			}
		case c.Op == fake.OpEdit && len(posts) > 0 && c.Ref == posts[0].Ref:
			first = c.Message
		}
	}
	return posts, first
}

func (f *front) waitFirst(t *testing.T, what string, ok func(surface.Message) bool) surface.Message {
	t.Helper()
	var m surface.Message
	waitFor(t, what, func() bool {
		_, m = f.dmPosts()
		return ok(m)
	})
	return m
}

func (f *front) twoThreads(t *testing.T) (a, b control.ThreadID) {
	t.Helper()
	f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
	f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git second")
	ths := f.threads()
	return f.threadAt(t, ths[0].Opened), f.threadAt(t, ths[1].Opened)
}

func TestOneDirectMessagePerOutage(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, b := f.twoThreads(t)
	stop := f.running(t)
	f.lost(t, a, 1)
	f.waitFirst(t, "the alert", func(m surface.Message) bool { return strings.Contains(m.Text, "#1 first") })
	f.stopped(t, b)
	m := f.waitFirst(t, "the second thread in it", func(m surface.Message) bool { return strings.Contains(m.Text, "#2 second") })
	if !strings.Contains(m.Text, "agent on test-instance is logged out") || !strings.Contains(m.Text, "Stopped threads (2)") {
		t.Fatalf("alert %q", m.Text)
	}
	stop()
	f.running(t) // a restart during the outage
	f.event(t, a, "text", map[string]any{"text": "marker", "final": false})
	f.postedUntil(t, a, "marker")
	time.Sleep(50 * time.Millisecond)
	if posts, _ := f.dmPosts(); len(posts) != 1 {
		t.Fatalf("%d direct messages, want 1", len(posts))
	}
}

func TestAtMostFifteenThreadsAreNamed(t *testing.T) {
	f := newFront(t, workspaces{}, withRelay(&fakeRelay{}))
	f.running(t)
	ids := make([]control.ThreadID, 17)
	for i := range ids {
		ids[i] = f.bareThread(t, fmt.Sprintf("k%02d", i+1))
	}
	f.lost(t, ids[0], 1)
	for _, id := range ids[1:] {
		f.stopped(t, id)
	}
	m := f.waitFirst(t, "17 threads", func(m surface.Message) bool { return strings.Contains(m.Text, "Stopped threads (17)") })
	if n := strings.Count(m.Text, "\n- "); n != 15 || !strings.Contains(m.Text, "…and 2 more.") || strings.Contains(m.Text, fmt.Sprintf("#%d", ids[15])) {
		t.Fatalf("alert names %d threads:\n%s", n, m.Text)
	}
}

func TestTheOwnersCodeGoesToTheRelayAndTheLinkStaysInTheDirectConversation(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, b := f.twoThreads(t)
	f.running(t)
	f.lost(t, a, 1)
	f.stopped(t, b)
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Link: link, Expires: time.Now().Add(10 * time.Minute)})
	m := f.waitFirst(t, "the link", func(m surface.Message) bool { return strings.Contains(m.Text, link) })
	if len(m.Choices) != 1 {
		t.Fatalf("alert %+v, want the button", m)
	}
	f.fe.Message(f.driver.InjectDirect(owner, "CODE-123"))
	if _, codes := r.got(); len(codes) != 1 || codes[0] != "chat:"+owner+" agent CODE-123" {
		t.Fatalf("codes handed to the relay: %v", codes)
	}
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Done: relay.LoggedIn})
	f.raw(t, a, "login_restored", map[string]any{"agent": "agent", "outage": 1, "threads": []control.ThreadID{a, b}, "scopes": []string{channel}})
	m = f.waitFirst(t, "the end of the outage", func(m surface.Message) bool { return strings.Contains(m.Text, "Logged in again.") })
	if strings.Contains(m.Text, link) || len(m.Choices) != 0 {
		t.Fatalf("alert after the login %+v, want no link and no button", m)
	}
	for _, id := range []control.ThreadID{a, b} {
		f.postedUntil(t, id, "The agent is logged in again. Send your last message again to go on.")
	}
	posts, _ := f.dmPosts()
	if last := posts[len(posts)-1].Message.Text; last != "Logged in again." {
		t.Fatalf("result in the direct conversation %q", last)
	}
	if got := f.runner.handed(); len(got) != 0 {
		t.Fatalf("sent to the agent: %v", got)
	}
	// The link is nowhere else: no other conversation, no outbox row, no
	// stored outage.
	for _, conv := range []surface.Conversation{chanConv, f.conv(a), f.conv(b)} {
		for _, c := range f.driver.Calls(conv) {
			if strings.Contains(c.Message.Text, "SECRETLINK") {
				t.Fatalf("the link is in %+v", conv)
			}
		}
	}
	for _, table := range []string{"chat_outbox", "chat_logins", "events", "inbox"} {
		var n int
		f.fe.Outbox.Store.DB.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE CAST(` + map[string]string{
			"chat_outbox": "message", "chat_logins": "threads || dm_ref", "events": "data", "inbox": "data"}[table] + ` AS TEXT) LIKE '%SECRETLINK%'`).Scan(&n)
		if n != 0 {
			t.Fatalf("the link is in %s", table)
		}
	}
}

func TestDirectMessagesThatAreNoCode(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, _ := f.twoThreads(t)
	f.running(t)
	f.lost(t, a, 1)
	f.waitFirst(t, "the alert", func(m surface.Message) bool { return m.Text != "" })
	before := len(f.log.lines())
	f.fe.Message(f.driver.InjectDirect(owner, "CODE-1")) // no link yet: nothing awaited
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Link: link, Expires: time.Now().Add(time.Minute)})
	f.fe.Message(f.driver.InjectDirect(owner, "two words"))
	f.fe.Message(f.driver.InjectDirect("someone", "CODE-2 secret words"))
	posts, _ := f.dmPosts()
	var replies []string
	for _, p := range posts[1:] {
		replies = append(replies, p.Message.Text)
	}
	if got := strings.Join(replies, "|"); got != "Not waiting for a code.|That does not look like a code: a code is one word without spaces." {
		t.Fatalf("replies %q", got)
	}
	if c := f.driver.Calls(f.driver.InjectDirect("someone", "x").Ref.Conv); len(c) != 0 {
		t.Fatalf("a reply to someone else: %+v", c)
	}
	if _, codes := r.got(); len(codes) != 0 {
		t.Fatalf("codes handed to the relay: %v", codes)
	}
	logs := strings.Join(f.log.lines()[before:], "\n")
	if !strings.Contains(logs, "author=someone") || strings.Contains(logs, "secret") {
		t.Fatalf("log %q, want the sender and not the text", logs)
	}
}

func TestExpiredLinkKeepsItsButtonAlsoAfterARestart(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, _ := f.twoThreads(t)
	stop := f.running(t)
	f.lost(t, a, 1)
	f.waitFirst(t, "the alert", func(m surface.Message) bool { return m.Text != "" })
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Link: link, Expires: time.Now().Add(time.Minute)})
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Done: relay.Expired})
	m := f.waitFirst(t, "the expiry", func(m surface.Message) bool { return strings.Contains(m.Text, "expired") })
	if len(m.Choices) != 1 || strings.Contains(m.Text, link) {
		t.Fatalf("alert %+v", m)
	}
	stop()
	// A new front end, as after a restart: the button on the same message.
	f.fe.login = nil
	f.fe.loginOnce = sync.Once{}
	f.running(t)
	f.event(t, a, "text", map[string]any{"text": "marker", "final": false})
	f.postedUntil(t, a, "marker")
	time.Sleep(50 * time.Millisecond)
	if _, m := f.dmPosts(); !strings.Contains(m.Text, "expired") || len(m.Choices) != 1 {
		t.Fatalf("alert after the restart %+v, want it still to say the link expired", m)
	}
	posts, _ := f.dmPosts()
	f.fe.Choice(surface.Selection{Ref: posts[0].Ref, Author: fake.Person("someone"), Token: m.Choices[0].Token})
	f.fe.Choice(surface.Selection{Ref: posts[0].Ref, Author: fake.Person(owner), Token: m.Choices[0].Token})
	if starts, _ := r.got(); len(starts) != 1 || starts[0] != "chat:"+owner+" agent" {
		t.Fatalf("relay starts %v", starts)
	}
	fresh := link + "&fresh"
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Link: fresh, Expires: time.Now().Add(time.Minute)})
	f.waitFirst(t, "the fresh link", func(m surface.Message) bool { return strings.Contains(m.Text, fresh) })
	if posts, _ := f.dmPosts(); len(posts) != 1 {
		t.Fatalf("%d direct messages", len(posts))
	}
}

func TestRelayOffStillSendsTheAlert(t *testing.T) {
	f := newFront(t, workspaces{}, withRelay(nil))
	a, _ := f.twoThreads(t)
	f.running(t)
	f.lost(t, a, 1)
	m := f.waitFirst(t, "the alert", func(m surface.Message) bool { return m.Text != "" })
	if len(m.Choices) != 0 || !strings.Contains(m.Text, "at the host") {
		t.Fatalf("alert %+v, want no link and no button", m)
	}
}

func TestUndeliverableAlert(t *testing.T) {
	f := newFront(t, workspaces{}, withRelay(&fakeRelay{}))
	a, _ := f.twoThreads(t)
	f.driver.Undeliverable(owner)
	f.running(t)
	f.lost(t, a, 1)
	f.postedUntil(t, a, "The turn failed: the agent is not logged in.")
	waitFor(t, "the log line", func() bool {
		return strings.Contains(strings.Join(f.log.lines(), "\n"), "the owner could not be reached")
	})
}

// A refused code is said in the direct conversation, and the next one
// still goes to the relay.
func TestARefusedCodeAndTheNextOne(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, _ := f.twoThreads(t)
	f.running(t)
	f.lost(t, a, 1)
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Link: link, Expires: time.Now().Add(time.Minute)})
	f.fe.Message(f.driver.InjectDirect(owner, "WRONG"))
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Refused: "Invalid code."})
	f.fe.Message(f.driver.InjectDirect(owner, "RIGHT"))
	if _, codes := r.got(); strings.Join(codes, "|") != "chat:"+owner+" agent WRONG|chat:"+owner+" agent RIGHT" {
		t.Fatalf("codes %v", codes)
	}
	posts, _ := f.dmPosts()
	if posts[len(posts)-1].Message.Text != "The code was refused: Invalid code." {
		t.Fatalf("last reply %q", posts[len(posts)-1].Message.Text)
	}
}

func TestDeviceLoginMessage(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, _ := f.twoThreads(t)
	f.running(t)
	f.lost(t, a, 1)
	expires := time.Date(2026, 1, 1, 12, 34, 0, 0, time.UTC)
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Link: link, ShowCode: "ABCD-12345", Expires: expires, NoCode: true})
	m := f.waitFirst(t, "device code", func(m surface.Message) bool { return strings.Contains(m.Text, "ABCD-12345") })
	for _, want := range []string{"Sign in at " + link, "Code to enter there: ABCD-12345", "Nothing to send back here: the login ends by itself once you have signed in, until 12:34 UTC."} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("missing %q in %q", want, m.Text)
		}
	}
	f.fe.Message(f.driver.InjectDirect(owner, "ABCD-12345"))
	posts, _ := f.dmPosts()
	if posts[len(posts)-1].Message.Text != "Not waiting for a code." {
		t.Fatalf("posts: %+v", posts)
	}
	if _, codes := r.got(); len(codes) != 0 {
		t.Fatalf("codes: %v", codes)
	}
}
