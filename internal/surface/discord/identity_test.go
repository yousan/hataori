package discord

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

const whoami = "GET /users/@me"

// made returns every request the fake received, in order.
func (f *fakeREST) made() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// The token is for the bot of the captures, and the config names another
// one. No call of the driver sends a request with it, before Run and after
// Run refused; the one request is the identity check (#566).
func TestNoCallIsMadeAsAnotherBot(t *testing.T) {
	ctx := context.Background()
	post := capture(t, "bot/rest-post-channel.json")
	path := post["request"].(map[string]any)["path"].(string) // /channels/<channel>/messages
	conv := surface.Conversation{Channel: strings.Split(path, "/")[2]}
	thread := surface.Conversation{Channel: conv.Channel, Thread: "1000000000000000003"}
	ref := surface.MessageRef{Conv: conv, ID: "1000000000000000010"}
	calls := map[string]func(*Driver) error{
		"Post": func(d *Driver) error {
			_, err := d.Post(ctx, conv, "1.0", surface.Message{Text: "a pending row of the outbox"})
			return err
		},
		"Edit":       func(d *Driver) error { return d.Edit(ctx, ref, surface.Message{Text: "edited"}) },
		"Delete":     func(d *Driver) error { return d.Delete(ctx, ref) },
		"SetStatus":  func(d *Driver) error { return d.SetStatus(ctx, ref, surface.Working) },
		"SetThread":  func(d *Driver) error { return d.SetThread(ctx, thread, surface.ThreadMeta{Archived: true}) },
		"OpenThread": func(d *Driver) error { _, err := d.OpenThread(ctx, conv, "a title"); return err },
		"OpenDirect": func(d *Driver) error { _, err := d.OpenDirect(ctx, "1000000000000000001"); return err },
		"Since":      func(d *Driver) error { _, err := d.Since(ctx, conv, "1"); return err },
		"Name":       func(d *Driver) error { _, err := d.Name(ctx, conv); return err },
	}
	for name, call := range calls {
		for _, ran := range []bool{false, true} {
			r := newFakeREST(t)
			r.serve(t, whoami, "bot/rest-users-me.json")
			r.serve(t, "POST "+path, "bot/rest-post-channel.json")
			d := testDriver(t, 1000000000000000999, r, newFakeGateway(t, payload(t, "bot/gw-01-READY.json")))
			if ran {
				if err := d.Run(ctx, newSink()); !errors.Is(err, surface.ErrOtherIdentity) {
					t.Fatalf("Run with the token of another bot: %v", err)
				}
			}
			for range 2 {
				if err := call(d); !errors.Is(err, surface.ErrOtherIdentity) || errors.Is(err, surface.ErrRefused) {
					t.Errorf("%s, after Run: %v: %v", name, ran, err)
				}
			}
			if got := r.made(); !slices.Equal(got, []string{whoami}) {
				t.Errorf("%s, after Run: %v: the service received %q, want the identity check only", name, ran, got)
			}
		}
	}
}

// A check that fails is not the answer to the call that waited for it: a
// 403 to the check does not refuse a post for good. The next call checks
// again, and the one after a check that passed does not.
func TestACallWaitsForTheIdentityCheck(t *testing.T) {
	r := newFakeREST(t)
	r.serveJSON(t, whoami, 503, map[string]any{"message": "service unavailable", "code": 0})
	r.serveJSON(t, whoami, 403, map[string]any{"message": "Missing Access", "code": 50001})
	r.serve(t, whoami, "bot/rest-users-me.json")
	post := capture(t, "bot/rest-post-channel.json")
	path := post["request"].(map[string]any)["path"].(string)
	r.serve(t, "POST "+path, "bot/rest-post-channel.json")
	d := testDriver(t, botID(t), r, newFakeGateway(t, payload(t, "bot/gw-01-READY.json")))
	send := func() error {
		_, err := d.Post(context.Background(), surface.Conversation{Channel: strings.Split(path, "/")[2]}, "1.0", surface.Message{Text: "hello"})
		return err
	}
	for range 2 {
		if err := send(); err == nil || errors.Is(err, surface.ErrRefused) || errors.Is(err, surface.ErrOtherIdentity) {
			t.Fatalf("posted while the check fails: %v", err)
		}
	}
	for range 2 {
		if err := send(); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := r.made(), []string{whoami, whoami, whoami, "POST " + path, "POST " + path}; !slices.Equal(got, want) {
		t.Fatalf("the service received %q, want %q", got, want)
	}
}

// frontEndOn is a front end on d with a core of its own that runs nothing,
// and its database.
func frontEndOn(t *testing.T, d *Driver) (*chat.FrontEnd, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := people.New([]people.Person{{Name: "owner"}}, "owner", nil)
	c := &core.Core{Store: st, Machine: &session.Machine{Store: st}, Log: lg, People: dir}
	return &chat.FrontEnd{Name: Name, Driver: d, API: c.As, People: dir, Trigger: "!go", Log: lg, RetryFirst: 5 * time.Millisecond,
		Outbox: &chat.Outbox{Store: st, Client: Name, Driver: d, Log: lg, Retry: 5 * time.Millisecond}}, st.DB
}

// The front end of a driver with another bot's token sends nothing but the
// identity check, ends with the reason, and leaves its outbox as it was,
// also the decorations that three failures drop (#566).
func TestFrontEndPostsNothingAsAnotherBot(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, whoami, "bot/rest-users-me.json")
	post := capture(t, "bot/rest-post-channel.json")
	path := post["request"].(map[string]any)["path"].(string)
	r.serve(t, "POST "+path, "bot/rest-post-channel.json")
	fe, db := frontEndOn(t, testDriver(t, 1000000000000000999, r, newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conv := surface.Conversation{Channel: strings.Split(path, "/")[2]}
	if err := fe.Outbox.Write(ctx,
		chat.Row{Event: 1, Conv: conv, Message: surface.Message{Text: "a pending row"}},
		chat.Row{Event: 2, Conv: conv, Message: surface.Message{Text: "a decoration"}, Decoration: true}); err != nil {
		t.Fatal(err)
	}
	err := fe.Run(ctx)
	if !errors.Is(err, surface.ErrOtherIdentity) || !strings.Contains(err.Error(), botID(t).String()) ||
		!strings.Contains(err.Error(), "1000000000000000999") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("Run: %v, want one line that names both bots", err)
	}
	if got := r.made(); !slices.Equal(got, []string{whoami}) {
		t.Fatalf("the service received %q, want the identity check only", got)
	}
	var pending int
	if err := db.QueryRow(`SELECT count(*) FROM chat_outbox WHERE state = 'pending' AND failures = 0`).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("%d rows pending as they were (%v), want 2", pending, err)
	}
}

// A request before the connection that fails in a way that passes, at the
// identity check or at the registration of the commands, is made again
// until the front end is connected (#567). The 503s are written by hand.
func TestFrontEndOutlivesFailedRequestsAtStart(t *testing.T) {
	r := newFakeREST(t)
	down := map[string]any{"message": "service unavailable", "code": 0}
	r.serveJSON(t, whoami, 503, down)
	r.serve(t, whoami, "bot/rest-users-me.json")
	r.serveJSON(t, "GET /applications/@me", 503, down)
	r.serve(t, "GET /applications/@me", "commands/rest-application.json")
	r.serveJSON(t, "GET "+commandsKey(t), 503, down)
	r.serve(t, "GET "+commandsKey(t), "commands/rest-commands-after.json")
	r.serveJSON(t, "PUT "+commandsKey(t), 503, down)
	r.serve(t, "PUT "+commandsKey(t), "commands/rest-commands-set.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	fe, _ := frontEndOn(t, testDriver(t, botID(t), r, g))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fe.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	select {
	case <-g.connected:
	case err := <-done:
		done <- err
		t.Fatalf("Run returned: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatalf("no session; the service received %q", r.made())
	}
	if n := r.called(whoami); n < 2 {
		t.Fatalf("%d identity checks, want the failed one and the one that passed", n)
	}
	if n := r.called("PUT "); n < 2 {
		t.Fatalf("%d registrations, want the failed one and the one that passed", n)
	}
}

// An answer that names no bot, as a proxy in between may give, says nothing
// about the token: the check is made again. Written by hand.
func TestAnAnswerWithoutABotIsNoOtherBot(t *testing.T) {
	r := newFakeREST(t)
	r.serveJSON(t, whoami, 200, map[string]any{})
	r.serve(t, whoami, "bot/rest-users-me.json")
	d := testDriver(t, botID(t), r, newFakeGateway(t, payload(t, "bot/gw-01-READY.json")))
	if err := d.CheckIdentity(context.Background()); err == nil || errors.Is(err, surface.ErrOtherIdentity) {
		t.Fatalf("CheckIdentity after an answer without a bot: %v", err)
	}
	if err := d.CheckIdentity(context.Background()); err != nil {
		t.Fatalf("CheckIdentity after that: %v", err)
	}
}

// closer is a request's body that knows whether it was closed.
type closer struct {
	io.Reader
	closed bool
}

func (c *closer) Close() error { c.closed = true; return nil }

// A refused request's body is closed, as http.RoundTripper asks for.
func TestARefusedRequestsBodyIsClosed(t *testing.T) {
	i := &identity{bot: 1, checking: make(chan struct{}, 1)}
	i.refuse(2)
	body := &closer{Reader: strings.NewReader("a file")}
	req, err := http.NewRequest("POST", "http://127.0.0.1:1", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.RoundTrip(req); !errors.Is(err, surface.ErrOtherIdentity) || !body.closed {
		t.Fatalf("RoundTrip: %v, body closed: %v", err, body.closed)
	}
}
