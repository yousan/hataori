package chat

import (
	"context"
	"encoding/json"
	"errors"
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

func (r *fakeRelay) Start(_ context.Context, caller, agent string, outage int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, fmt.Sprintf("%s %s#%d", caller, agent, outage))
	return nil
}

func (r *fakeRelay) Code(_ context.Context, caller, agent string, outage int64, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, fmt.Sprintf("%s %s#%d %s", caller, agent, outage, code))
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
	f.lostIn(t, id, outage, "")
}

// lostIn is lost of the login in a configuration directory.
func (f *front) lostIn(t *testing.T, id control.ThreadID, outage int, dir string) {
	t.Helper()
	if _, err := f.fe.Outbox.Store.DB.Exec(`INSERT INTO login_outages (id, agent, config_dir, opened_at) VALUES (?, 'agent', ?, 0)`, outage, dir); err != nil {
		t.Fatal(err)
	}
	f.joined(t, id, outage)
	f.event(t, id, "turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": "failure", "reason": "login", "background": 0})
	f.raw(t, id, "login_lost", map[string]any{"agent": "agent", "outage": outage, "thread": 1})
}

// joined records the thread as stopped by the outage, as the core does with
// the end of its turn.
func (f *front) joined(t *testing.T, id control.ThreadID, outage int) {
	t.Helper()
	if _, err := f.fe.Outbox.Store.DB.Exec(`INSERT INTO login_outage_threads (outage_id, thread_id) VALUES (?, ?)`, outage, id); err != nil {
		t.Fatal(err)
	}
}

func (f *front) stopped(t *testing.T, id control.ThreadID, outage int) {
	t.Helper()
	f.joined(t, id, outage)
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
	f.stopped(t, b, 1)
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
		f.stopped(t, id, 1)
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
	f.stopped(t, b, 1)
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link, Expires: time.Now().Add(10 * time.Minute)})
	m := f.waitFirst(t, "the link", func(m surface.Message) bool { return strings.Contains(m.Text, link) })
	if len(m.Choices) != 1 {
		t.Fatalf("alert %+v, want the button", m)
	}
	f.fe.Message(f.driver.InjectDirect(owner, "CODE-123"))
	if _, codes := r.got(); len(codes) != 1 || codes[0] != "chat:"+owner+" agent#1 CODE-123" {
		t.Fatalf("codes handed to the relay: %v", codes)
	}
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Done: relay.LoggedIn})
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
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link, Expires: time.Now().Add(time.Minute)})
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
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link, Expires: time.Now().Add(time.Minute)})
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Done: relay.Expired})
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
	if starts, _ := r.got(); len(starts) != 1 || starts[0] != "chat:"+owner+" agent#1" {
		t.Fatalf("relay starts %v", starts)
	}
	fresh := link + "&fresh"
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: fresh, Expires: time.Now().Add(time.Minute)})
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
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link, Expires: time.Now().Add(time.Minute)})
	f.fe.Message(f.driver.InjectDirect(owner, "WRONG"))
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Refused: "Invalid code."})
	f.fe.Message(f.driver.InjectDirect(owner, "RIGHT"))
	if _, codes := r.got(); strings.Join(codes, "|") != "chat:"+owner+" agent#1 WRONG|chat:"+owner+" agent#1 RIGHT" {
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
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link, ShowCode: "ABCD-12345", Expires: expires, NoCode: true})
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

// Two logins of one agent are two messages, each with its own threads, link
// and button, and a code goes to the one it answers.
func TestTwoLoginsOfOneAgent(t *testing.T) {
	r := &fakeRelay{}
	f := newFront(t, workspaces{}, withRelay(r))
	a, b := f.twoThreads(t)
	f.running(t)
	f.lost(t, a, 1)
	f.waitFirst(t, "the first alert", func(m surface.Message) bool { return strings.Contains(m.Text, "#1 first") })
	f.lostIn(t, b, 2, "/home/alice/.agent_work")
	// alerts returns the two messages, as they are now.
	alerts := func() (refs []surface.MessageRef, texts []surface.Message) {
		for _, c := range f.driver.Calls(f.dm()) {
			switch {
			case c.Op == fake.OpPost && strings.Contains(c.Message.Text, "is logged out"):
				refs, texts = append(refs, c.Ref), append(texts, c.Message)
			case c.Op == fake.OpEdit:
				for i := range refs {
					if c.Ref == refs[i] {
						texts[i] = c.Message
					}
				}
			}
		}
		return refs, texts
	}
	var refs []surface.MessageRef
	var ms []surface.Message
	waitAlerts := func(what string, ok func() bool) {
		t.Helper()
		waitFor(t, what, func() bool { refs, ms = alerts(); return len(ms) == 2 && ok() })
	}
	waitAlerts("the second alert", func() bool { return strings.Contains(ms[1].Text, "#2 second") })
	if strings.Contains(ms[0].Text, "#2 second") || strings.Contains(ms[1].Text, "#1 first") ||
		!strings.Contains(ms[0].Text, "Stopped threads (1)") || !strings.Contains(ms[1].Text, "Stopped threads (1)") {
		t.Fatalf("alerts:\n%s\n--\n%s", ms[0].Text, ms[1].Text)
	}
	if strings.Contains(ms[0].Text, "directory") || !strings.Contains(ms[1].Text, "Its configuration directory:\n"+f.driver.Verbatim("/home/alice/.agent_work")+"\n") {
		t.Fatalf("alerts:\n%s\n--\n%s", ms[0].Text, ms[1].Text)
	}

	expires := time.Now().Add(10 * time.Minute)
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link + "&own", Expires: expires})
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 2, Link: link + "&work", Expires: expires})
	waitAlerts("the links", func() bool {
		return strings.Contains(ms[0].Text, "&own") && strings.Contains(ms[1].Text, "&work")
	})
	// Each button is its own login's.
	for _, m := range ms {
		f.fe.Choice(surface.Selection{Ref: refs[0], Author: fake.Person(owner), Token: m.Choices[0].Token})
	}
	// One from before the upgrade starts the agent's own.
	f.fe.Choice(surface.Selection{Ref: refs[0], Author: fake.Person(owner), Token: "login:agent"})
	by := "chat:" + owner
	if starts, _ := r.got(); strings.Join(starts, "|") != by+" agent#1|"+by+" agent#2|"+by+" agent#0" {
		t.Fatalf("relay starts %v", starts)
	}

	// Which of the two a bare code is for is not guessed.
	f.fe.Message(f.driver.InjectDirect(owner, "CODE-X"))
	if _, codes := r.got(); len(codes) != 0 {
		t.Fatalf("codes %v", codes)
	}
	posts, _ := f.dmPosts()
	if got := posts[len(posts)-1].Message.Text; got != "More than one login waits for a code: send it as a reply to the message whose link you used." {
		t.Fatalf("reply to a bare code %q", got)
	}
	answer := func(ref surface.MessageRef, code string) {
		in := f.driver.InjectDirect(owner, code)
		in.ReplyTo = &ref
		f.fe.Message(in)
	}
	answer(refs[1], "CODE-2")
	answer(refs[0], "CODE-1")
	if _, codes := r.got(); strings.Join(codes, "|") != by+" agent#2 CODE-2|"+by+" agent#1 CODE-1" {
		t.Fatalf("codes %v", codes)
	}

	// What comes of it is said as a reply to the login's message.
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 2, Refused: "Invalid code."})
	posts, _ = f.dmPosts()
	last := posts[len(posts)-1].Message
	if last.Text != "The code was refused: Invalid code." || last.ReplyTo == nil || *last.ReplyTo != refs[1] {
		t.Fatalf("refusal %+v", last)
	}
	// A code that replies to that is for the same login.
	answer(posts[len(posts)-1].Ref, "CODE-2B")
	if _, codes := r.got(); len(codes) != 3 || codes[2] != by+" agent#2 CODE-2B" {
		t.Fatalf("codes %v", codes)
	}
	// It is said without the reply when the login's message is gone.
	f.driver.Fail(fake.OpPost, 1, errors.New("unknown message"))
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 2, Refused: "Invalid code again."})
	posts, _ = f.dmPosts()
	last = posts[len(posts)-1].Message
	if last.Text != "The code was refused: Invalid code again." || last.ReplyTo != nil {
		t.Fatalf("refusal with the login's message gone %+v", last)
	}

	// One is back: the other still waits, and takes a bare code again, but
	// not the code that answers the one that is back.
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 2, Done: relay.LoggedIn})
	f.raw(t, b, "login_restored", map[string]any{"agent": "agent", "outage": 2, "threads": []control.ThreadID{b}, "scopes": []string{channel}})
	waitAlerts("the end of one", func() bool { return strings.Contains(ms[1].Text, "Logged in again.") })
	if !strings.Contains(ms[0].Text, "&own") || strings.Contains(ms[0].Text, "Logged in again.") {
		t.Fatalf("the other alert now:\n%s", ms[0].Text)
	}
	answer(refs[1], "CODE-3")
	posts, _ = f.dmPosts()
	if got := posts[len(posts)-1].Message.Text; got != "Not waiting for a code." {
		t.Fatalf("a code for a login that is back: %q", got)
	}
	f.fe.Message(f.driver.InjectDirect(owner, "CODE-4"))
	if _, codes := r.got(); len(codes) != 4 || codes[3] != by+" agent#1 CODE-4" {
		t.Fatalf("codes %v", codes)
	}

	// How the other's flow ends is stored for it only.
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Done: relay.Expired})
	waitAlerts("the expiry", func() bool { return strings.Contains(ms[0].Text, "expired") })
	f.lostIn(t, b, 3, "/home/alice/.agent_third")
	waitFor(t, "the third alert", func() bool { refs, ms = alerts(); return len(ms) == 3 })
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 3, Link: link + "&third", Expires: expires})
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 3, Done: relay.Failed, Detail: "no"})
	for id, want := range map[int]string{1: string(relay.Expired), 3: string(relay.Failed)} {
		var ended string
		if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT ended FROM chat_logins WHERE outage = ?`, id).Scan(&ended); err != nil || ended != want {
			t.Errorf("the end stored for outage %d: %q (%v), want %q", id, ended, err, want)
		}
	}
	// A flow ended by a login elsewhere waits for nothing any more.
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 3, Link: link + "&fourth", Expires: expires})
	f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 3, Done: relay.Restored})
	if l := f.fe.loginState(); l.flows[loginKey{"agent", 3}] != nil {
		t.Error("the flow of a login that is back is kept")
	}
}
