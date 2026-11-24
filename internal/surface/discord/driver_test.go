package discord

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// botID is the test bot's id in the captures.
func botID(t *testing.T) snowflake.ID {
	return snowflake.MustParse(payload(t, "bot/rest-users-me.json")["id"].(string))
}

func id(v any) string { return v.(string) }

// testDriver is a driver on the fakes. One without a gateway is not run, and
// its identity counts as checked.
func testDriver(t *testing.T, self snowflake.ID, r *fakeREST, g *fakeGateway) *Driver {
	if g == nil {
		d := newDriver(self, "test-token", r.URL, "ws://127.0.0.1:1", nil)
		d.identity.passed.Store(true)
		return d
	}
	return newDriver(self, "test-token", r.URL, g.URL(), nil)
}

// deliver feeds one gateway capture to the driver as the gateway would.
func deliver(t *testing.T, d *Driver, s *sink, name string) {
	t.Helper()
	c := capture(t, name)
	if err := d.dispatch(context.Background(), c["t"].(string), bytes.NewReader(raw(t, c["d"])), s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestDecodeInboundCaptures(t *testing.T) {
	r := newFakeREST(t)
	// A driver that is not the bot in the captures, so that the bot
	// account's messages are delivered as any other bot's.
	d := testDriver(t, 1, r, nil)
	s := newSink()
	deliver(t, d, s, "bot/gw-05-THREAD_CREATE.json")
	thread := payload(t, "bot/gw-05-THREAD_CREATE.json")

	cases := []struct {
		file    string
		kind    surface.AuthorKind
		author  string // the author's id key in the payload
		thread  bool
		direct  bool
		service bool
	}{
		{"person/gw-08-MESSAGE_CREATE.json", surface.Person, "author.id", false, false, false},
		{"person/gw-11-MESSAGE_CREATE.json", surface.Webhook, "webhook_id", false, false, false},
		{"bot/gw-03-MESSAGE_CREATE.json", surface.Bot, "author.id", false, false, false},
		{"bot/gw-08-MESSAGE_CREATE.json", surface.Bot, "author.id", true, false, false},
		{"bot/gw-07-MESSAGE_CREATE.json", surface.Bot, "author.id", true, false, true},  // the thread's starter
		{"bot/gw-12-MESSAGE_CREATE.json", surface.Bot, "author.id", true, false, false}, // with an attachment
		{"person/gw-07-MESSAGE_CREATE.json", surface.Person, "author.id", false, true, false},
	}
	for _, c := range cases {
		s.messages = nil
		deliver(t, d, s, c.file)
		if len(s.messages) != 1 {
			t.Errorf("%s: %d messages delivered", c.file, len(s.messages))
			continue
		}
		in, p := s.messages[0], payload(t, c.file)
		var wantAuthor string
		if c.author == "webhook_id" {
			wantAuthor = id(p["webhook_id"])
		} else {
			wantAuthor = id(p["author"].(map[string]any)["id"])
		}
		want := surface.Conversation{Channel: id(p["channel_id"])}
		if c.thread {
			want = surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(p["channel_id"])}
		}
		if in.Author.Kind != c.kind || in.Author.ID != wantAuthor || in.Author.Name == "" {
			t.Errorf("%s: author %+v, want kind %d id %s", c.file, in.Author, c.kind, wantAuthor)
		}
		if in.Ref != (surface.MessageRef{Conv: want, ID: id(p["id"])}) || in.Cursor != id(p["id"]) {
			t.Errorf("%s: ref %+v cursor %s, want %+v %s", c.file, in.Ref, in.Cursor, want, id(p["id"]))
		}
		if in.Direct != c.direct || in.Service != c.service {
			t.Errorf("%s: direct %v service %v", c.file, in.Direct, in.Service)
		}
		if in.Text != p["content"].(string) || in.Time.IsZero() {
			t.Errorf("%s: text %q time %v", c.file, in.Text, in.Time)
		}
	}
	if r.called("GET /channels/") != 0 {
		t.Error("the thread's channel was looked up although THREAD_CREATE named it")
	}
}

// A message in a thread the driver never saw being created: its channel is
// looked up once.
func TestThreadChannelLookedUp(t *testing.T) {
	r := newFakeREST(t)
	th := payload(t, "bot/rest-thread-from-message.json")
	r.serveJSON(t, "GET /channels/"+id(th["id"]), 200, th)
	d := testDriver(t, 1, r, nil)
	s := newSink()
	deliver(t, d, s, "bot/gw-08-MESSAGE_CREATE.json")
	deliver(t, d, s, "bot/gw-09-MESSAGE_CREATE.json")
	if len(s.messages) != 2 || s.messages[0].Ref.Conv.Channel != id(th["parent_id"]) || s.messages[1].Ref.Conv.Thread != id(th["id"]) {
		t.Fatalf("delivered %+v", s.messages)
	}
	if n := r.called("GET /channels/"); n != 1 {
		t.Fatalf("looked up %d times", n)
	}
}

func TestOwnMessagesNotDelivered(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	s := newSink()
	deliver(t, d, s, "bot/gw-05-THREAD_CREATE.json")
	for _, f := range []string{"bot/gw-03-MESSAGE_CREATE.json", "bot/gw-08-MESSAGE_CREATE.json", "person/gw-03-MESSAGE_CREATE.json"} {
		deliver(t, d, s, f)
	}
	deliver(t, d, s, "person/gw-08-MESSAGE_CREATE.json")
	deliver(t, d, s, "person/gw-11-MESSAGE_CREATE.json")
	if got := s.texts(); !slices.Equal(got, []string{"[hataori test] person", "[hataori test] webhook"}) {
		t.Fatalf("delivered %q", got)
	}
}

func TestRunRefusesAnotherBot(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, 1000000000000000999, r, g)
	err := d.Run(context.Background(), newSink())
	if !errors.Is(err, surface.ErrOtherIdentity) || !strings.Contains(err.Error(), botID(t).String()) || !strings.Contains(err.Error(), "1000000000000000999") {
		t.Fatalf("Run: %v", err)
	}
	if g.identify != 0 {
		t.Fatal("connected to the gateway as the wrong bot")
	}

	// The token's bot is right, but the gateway logs in as another.
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	ready := payload(t, "bot/gw-01-READY.json")
	ready["user"] = map[string]any{"id": "1000000000000000998", "username": "other", "bot": true}
	g2 := newFakeGateway(t, ready)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d = testDriver(t, botID(t), r, g2)
	err = d.Run(ctx, newSink())
	if !errors.Is(err, surface.ErrOtherIdentity) || !strings.Contains(err.Error(), "1000000000000000998") || !strings.Contains(err.Error(), botID(t).String()) {
		t.Fatalf("Run: %v", err)
	}
	n := r.called("")
	if err := d.CheckIdentity(context.Background()); !errors.Is(err, surface.ErrOtherIdentity) {
		t.Fatalf("CheckIdentity after that: %v", err)
	}
	if _, err := d.OpenDirect(context.Background(), "1000000000000000001"); !errors.Is(err, surface.ErrOtherIdentity) || r.called("") != n {
		t.Fatalf("a call after that: %v, with %d requests", err, r.called("")-n)
	}
}

// person returns the person's channel message with text and its own id.
func person(t *testing.T, text string, n int) map[string]any {
	p := payload(t, "person/gw-08-MESSAGE_CREATE.json")
	p["content"] = text
	p["id"] = (snowflake.MustParse(id(p["id"])) + snowflake.ID(n)).String()
	return p
}

func TestReconnectKeepsOrder(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, botID(t), r, g)
	s := newSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()

	wait := func(what string, c <-chan struct{}) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(20 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait("the first session", g.connected)
	g.event("MESSAGE_CREATE", person(t, "1", 1))
	g.event("MESSAGE_CREATE", person(t, "2", 2))
	wait("message 1", s.got)
	wait("message 2", s.got)
	g.drop()
	wait("the second session", g.connected)
	g.event("MESSAGE_CREATE", person(t, "3", 3))
	g.event("MESSAGE_CREATE", person(t, "4", 4))
	wait("message 3", s.got)
	wait("message 4", s.got)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := s.texts(); !slices.Equal(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("delivered %q", got)
	}
	if g.identify+g.resume != 2 {
		t.Fatalf("%d identifies and %d resumes", g.identify, g.resume)
	}
}

// reconnectedSink records messages and Connected calls in one log.
type reconnectedSink struct {
	*sink
	log []string // "connected", or a message's text
}

func (s *reconnectedSink) Connected() {
	s.mu.Lock()
	s.log = append(s.log, "connected")
	s.mu.Unlock()
	s.got <- struct{}{}
}

func (s *reconnectedSink) Message(in surface.Inbound) {
	s.mu.Lock()
	s.log = append(s.log, in.Text)
	s.mu.Unlock()
	s.sink.Message(in)
}

// Run tells a sink that wants to know when a session is ready: the first
// one, a resumed one, and a new one after the service refused to resume.
func TestRunTellsSinkWhenConnected(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, botID(t), r, g)
	s := &reconnectedSink{sink: newSink()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()

	wait := func(what string, c <-chan struct{}) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(20 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait("the first session", g.connected)
	g.event("MESSAGE_CREATE", person(t, "1", 1))
	wait("connected", s.got)
	wait("message 1", s.got)
	g.drop() // resumed
	wait("the resumed session", g.connected)
	g.event("MESSAGE_CREATE", person(t, "2", 2))
	wait("connected again", s.got)
	wait("message 2", s.got)
	g.mu.Lock()
	g.refuseResume = true
	g.mu.Unlock()
	g.drop() // the resume is refused: a new session
	wait("the new session", g.connected)
	g.event("MESSAGE_CREATE", person(t, "3", 3))
	wait("connected a third time", s.got)
	wait("message 3", s.got)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"connected", "1", "connected", "2", "connected", "3"}
	if !slices.Equal(s.log, want) {
		t.Fatalf("got %q, want %q", s.log, want)
	}
	if g.identify != 2 || g.resume != 1 {
		t.Fatalf("%d identifies and %d resumes, want 2 and 1", g.identify, g.resume)
	}
}

func TestTokenOnlyFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	os.Mkdir(config, 0o700)
	os.WriteFile(filepath.Join(config, "discord-token"), []byte("test-token\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "outside"), []byte("other-token\n"), 0o600)
	os.Symlink(filepath.Join(dir, "outside"), filepath.Join(config, "link"))
	root, err := os.OpenRoot(config)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	drv, err := New(root, []byte(`{"bot_id": "1000000000000000001", "token_file": "discord-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	if drv.(*Driver).token != "test-token" {
		t.Fatal("token not read")
	}
	for _, f := range []string{"../outside", filepath.Join(dir, "outside"), "link", "", "missing"} {
		if _, err := New(root, []byte(`{"bot_id": "1000000000000000001", "token_file": "`+f+`"}`)); err == nil {
			t.Errorf("token_file %q accepted", f)
		}
	}
	if _, err := New(root, []byte(`{"bot_id": "x", "token_file": "discord-token"}`)); err == nil {
		t.Error("a bad bot_id was accepted")
	}
	if drv.(*Driver).prefix != "" {
		t.Errorf("prefix %q without command_prefix", drv.(*Driver).prefix)
	}
	drv, err = New(root, []byte(`{"bot_id": "1000000000000000001", "token_file": "discord-token", "command_prefix": " stg-\n"}`))
	if err != nil || drv.(*Driver).prefix != "stg-" {
		t.Fatalf("command_prefix: %v", err)
	}
	for _, p := range []string{"Stg-", "s g", "stg/", strings.Repeat("a", 32)} {
		if _, err := New(root, []byte(`{"bot_id": "1000000000000000001", "token_file": "discord-token", "command_prefix": "`+p+`"}`)); err == nil {
			t.Errorf("command_prefix %q accepted", p)
		}
	}
}

// A connection that drops just before Run ends is not reopened afterwards.
func TestNoReconnectAfterRun(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, botID(t), r, g)
	s := newSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()
	select {
	case <-g.connected:
	case <-time.After(20 * time.Second):
		t.Fatal("no session")
	}
	g.drop()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	time.Sleep(3 * time.Second) // longer than the first reconnect delay
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.identify+g.resume != 1 || g.conn != nil {
		t.Fatalf("reconnected after Run returned: %d identifies, %d resumes", g.identify, g.resume)
	}
}

// A thread's name is told when the thread is created and when it is
// renamed, with the thread's channel.
func TestThreadNamesTold(t *testing.T) {
	d := testDriver(t, 1, newFakeREST(t), nil)
	s := newSink()
	created, updated := payload(t, "bot/gw-05-THREAD_CREATE.json"), payload(t, "bot/gw-17-THREAD_UPDATE.json")
	deliver(t, d, s, "bot/gw-05-THREAD_CREATE.json")
	deliver(t, d, s, "bot/gw-17-THREAD_UPDATE.json")
	conv := surface.Conversation{Channel: id(created["parent_id"]), Thread: id(created["id"])}
	want := []renaming{{conv, created["name"].(string), 0}, {conv, updated["name"].(string), 0}}
	if !slices.Equal(s.renamed, want) || want[0].name == want[1].name {
		t.Fatalf("told %+v, want %+v", s.renamed, want)
	}
}

// Run tells the sink a thread's name when it is created and when it is
// renamed, in order with the messages (#614).
func TestRunTellsThreadNames(t *testing.T) {
	r, g := newFakeREST(t), newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	r.serveJSON(t, "GET /users/@me", 200, payload(t, "bot/rest-users-me.json"))
	d := testDriver(t, botID(t), r, g)
	s := newSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()
	<-g.connected
	created, updated := payload(t, "bot/gw-05-THREAD_CREATE.json"), payload(t, "bot/gw-17-THREAD_UPDATE.json")
	g.event("THREAD_CREATE", created)
	g.event("MESSAGE_CREATE", person(t, "between the names", 0))
	g.event("THREAD_UPDATE", updated)
	g.event("MESSAGE_CREATE", person(t, "after them", 1))
	for range 2 {
		select {
		case <-s.got:
		case <-time.After(5 * time.Second):
			t.Fatal("no message")
		}
	}
	cancel()
	<-done
	conv := surface.Conversation{Channel: id(created["parent_id"]), Thread: id(created["id"])}
	want := []renaming{{conv, created["name"].(string), 0}, {conv, updated["name"].(string), 1}}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(s.renamed, want) {
		t.Fatalf("told %+v, want %+v", s.renamed, want)
	}
}
