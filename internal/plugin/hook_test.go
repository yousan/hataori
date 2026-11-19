package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// stub is an API that the tests' plugins never call.
type stub struct{ control.API }

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hookFixture is a control socket with a dispatcher of grants, as the
// daemon wires them, and the plugins that connect to it.
type hookFixture struct {
	t      *testing.T
	root   string
	srv    *wire.Server
	d      *Dispatcher
	log    *logBuffer
	grants []Grant
	tokens map[string]string
}

func newHookFixture(t *testing.T, grants ...Grant) *hookFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &hookFixture{t: t, root: root, log: &logBuffer{}, grants: grants, tokens: map[string]string{}}
	f.d = NewDispatcher(grants, slog.New(slog.NewTextHandler(f.log, nil)))
	srv, err := wire.Listen(root, stub{}, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Peered = func(api control.API, p *wire.Peer) { f.d.Peered(api, p) }
	f.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	as := BindAs(func(control.Caller) control.API { return stub{} })
	for _, g := range grants {
		tok, err := srv.Issue(as(g.Caller()))
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[g.ID] = tok
	}
	return f
}

// connect connects plugin id with handler h (nil: it answers every
// request not_supported) and waits until the dispatcher knows the
// connection.
func (f *hookFixture) connect(id string, h wire.Handler) *wire.Client {
	f.t.Helper()
	c, err := wire.DialTokenHandler(f.root, f.tokens[id], h)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.Close() })
	for deadline := time.Now().Add(5 * time.Second); f.d.conn(id) == nil; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			f.t.Fatalf("the dispatcher does not know %s's connection", id)
		}
	}
	return c
}

func grant(id, point string, priority int, conversation bool) Grant {
	return Grant{ID: id, Scopes: []string{"s"}, Conversation: conversation,
		Hooks: []Hook{{Point: point, Priority: priority, Timeout: 5 * time.Second}}}
}

// hookHandler answers hook requests with answer, and plugin_started and
// plugin_stopping with nothing.
func hookHandler(answer func(r control.HookRequest, raw json.RawMessage) (any, error)) wire.Handler {
	return func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		switch op {
		case control.OpHook:
			var r control.HookRequest
			if err := json.Unmarshal(args, &r); err != nil {
				return nil, err
			}
			return answer(r, args)
		case control.OpPluginStarted, control.OpPluginStopping:
			return nil, nil
		}
		return nil, wire.ErrNotSupported
	}
}

func ptr(s string) *string { return &s }

// Handlers on one point run by priority, higher first, then in the
// config's order, and each sees the text as the one before left it.
func TestHandlersRunInOrder(t *testing.T) {
	f := newHookFixture(t,
		grant("a", "message_sending", 0, true),
		grant("b", "message_sending", 10, true),
		grant("c", "message_sending", 0, true))
	var mu sync.Mutex
	var order, seen []string
	for _, id := range []string{"a", "b", "c"} {
		f.connect(id, hookHandler(func(r control.HookRequest, _ json.RawMessage) (any, error) {
			mu.Lock()
			order, seen = append(order, id), append(seen, r.Text)
			mu.Unlock()
			return control.HookAnswer{Action: control.HookContinue, Text: ptr(r.Text + "|" + id)}, nil
		}))
	}
	res := f.d.Run(context.Background(), control.HookCall{Point: "message_sending", Thread: 7, Scope: "s", Text: "x"})
	if res.Stopped || res.Text != "x|b|a|c" {
		t.Fatalf("result %+v, want the text x|b|a|c", res)
	}
	if want := []string{"b", "a", "c"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	if want := []string{"x", "x|b", "x|b|a"}; !slices.Equal(seen, want) {
		t.Fatalf("each saw %q, want %q", seen, want)
	}
}

// A handler that times out gets its point's policy, before_agent_run's
// block and message_sending's skip, and a plugin's on_failure replaces
// it. Skipped, the others' results are kept.
func TestTimeoutPolicy(t *testing.T) {
	for _, tc := range []struct {
		point     string
		onFailure Failure
		blocks    bool
	}{
		{"before_agent_run", "", true},
		{"before_agent_run", Skip, false},
		{"message_sending", "", false},
		{"message_sending", Block, true},
	} {
		t.Run(tc.point+"/"+string(tc.onFailure), func(t *testing.T) {
			first, slow, after := grant("first", tc.point, 20, true), grant("slow", tc.point, 10, true), grant("after", tc.point, 0, true)
			// The policy as Load makes it: the config's, else the point's.
			slow.Hooks[0].Timeout, slow.Hooks[0].OnFailure = 100*time.Millisecond, points[tc.point]
			if tc.onFailure != "" {
				slow.Hooks[0].OnFailure = tc.onFailure
			}
			f := newHookFixture(t, first, slow, after)
			change := kinds[tc.point].change
			f.connect("first", hookHandler(func(r control.HookRequest, _ json.RawMessage) (any, error) {
				if change {
					return control.HookAnswer{Action: control.HookContinue, Text: ptr(r.Text + "+first")}, nil
				}
				return control.HookAnswer{Action: control.HookContinue}, nil
			}))
			release := make(chan struct{})
			defer close(release)
			f.connect("slow", hookHandler(func(r control.HookRequest, _ json.RawMessage) (any, error) {
				<-release
				return control.HookAnswer{Action: control.HookStop, Reason: "late"}, nil
			}))
			var afterSaw []string
			var mu sync.Mutex
			f.connect("after", hookHandler(func(r control.HookRequest, _ json.RawMessage) (any, error) {
				mu.Lock()
				afterSaw = append(afterSaw, r.Text)
				mu.Unlock()
				return control.HookAnswer{Action: control.HookContinue}, nil
			}))
			res := f.d.Run(context.Background(), control.HookCall{Point: tc.point, Thread: 7, Scope: "s", Text: "x"})
			want := "x"
			if change {
				want = "x+first"
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.blocks {
				if !res.Stopped || !res.Failed || res.By != "slow" || !strings.Contains(res.Reason, "did not answer within 100ms") {
					t.Fatalf("result %+v, want stopped by slow's timeout", res)
				}
				if len(afterSaw) != 0 {
					t.Fatalf("the handler after a block was called: %q", afterSaw)
				}
				return
			}
			if res.Stopped || res.Text != want {
				t.Fatalf("result %+v, want %q passing", res, want)
			}
			if !slices.Equal(afterSaw, []string{want}) {
				t.Fatalf("the handler after the skipped one saw %q, want %q", afterSaw, want)
			}
			if !strings.Contains(f.log.String(), "plugin hook failed") || !strings.Contains(f.log.String(), "policy=skip") {
				t.Fatalf("the skip is not logged:\n%s", f.log)
			}
		})
	}
}

// An answer not_supported, a connection that closes during the call, a
// plugin that is not connected, and an answer the point does not allow
// each count as a failure, so the handler's policy applies.
func TestFailuresFollowThePolicy(t *testing.T) {
	type setup func(f *hookFixture, id string)
	cases := []struct {
		name   string
		point  string // empty: both points
		setup  setup
		reason string
	}{
		{"not_supported", "", func(f *hookFixture, id string) { f.connect(id, nil) }, "not_supported"},
		{"closed during the call", "", func(f *hookFixture, id string) {
			f.connect(id, hookHandler(func(control.HookRequest, json.RawMessage) (any, error) {
				// The plugin exits: its token is revoked, and its
				// connections close before it answers.
				f.srv.Revoke(f.tokens[id])
				time.Sleep(100 * time.Millisecond)
				return control.HookAnswer{Action: control.HookContinue}, nil
			}))
		}, "its connection closed"},
		{"not connected", "", func(*hookFixture, string) {}, "it is not connected"},
		{"an error", "", func(f *hookFixture, id string) {
			f.connect(id, hookHandler(func(control.HookRequest, json.RawMessage) (any, error) {
				return nil, context.Canceled
			}))
		}, "error code error"},
		{"stop where the point does not allow it", "before_prompt_build", func(f *hookFixture, id string) {
			f.connect(id, hookHandler(func(control.HookRequest, json.RawMessage) (any, error) {
				return control.HookAnswer{Action: control.HookStop, Reason: "no"}, nil
			}))
		}, "does not allow"},
		{"text where the point does not allow it", "before_agent_run", func(f *hookFixture, id string) {
			f.connect(id, hookHandler(func(control.HookRequest, json.RawMessage) (any, error) {
				return control.HookAnswer{Action: control.HookContinue, Text: ptr("other")}, nil
			}))
		}, "does not allow"},
		{"an unknown action", "", func(f *hookFixture, id string) {
			f.connect(id, hookHandler(func(control.HookRequest, json.RawMessage) (any, error) {
				return control.HookAnswer{Action: "maybe"}, nil
			}))
		}, "neither continue nor stop"},
	}
	for _, tc := range cases {
		for _, pc := range []struct {
			point  string
			blocks bool
		}{{"before_agent_run", true}, {"message_sending", false}, {"before_prompt_build", false}} {
			if tc.point != "" && tc.point != pc.point || tc.point == "" && pc.point == "before_prompt_build" {
				continue
			}
			for _, flip := range []bool{false, true} {
				blocks := pc.blocks != flip
				name := tc.name + "/" + pc.point + "/" + map[bool]string{false: "default", true: "on_failure"}[flip]
				t.Run(name, func(t *testing.T) {
					bad := grant("bad", pc.point, 10, true)
					bad.Hooks[0].OnFailure = points[pc.point]
					if flip {
						bad.Hooks[0].OnFailure = map[bool]Failure{true: Block, false: Skip}[blocks]
					}
					f := newHookFixture(t, bad, grant("next", pc.point, 0, true))
					called := make(chan string, 1)
					f.connect("next", hookHandler(func(r control.HookRequest, _ json.RawMessage) (any, error) {
						called <- r.Text
						return control.HookAnswer{Action: control.HookContinue}, nil
					}))
					tc.setup(f, "bad")
					res := f.d.Run(context.Background(), control.HookCall{Point: pc.point, Thread: 7, Scope: "s", Text: "x"})
					if blocks {
						if !res.Stopped || !res.Failed || res.By != "bad" || !strings.Contains(res.Reason, tc.reason) {
							t.Fatalf("result %+v, want stopped by bad, with %q", res, tc.reason)
						}
						select {
						case <-called:
							t.Fatal("the handler after a block was called")
						default:
						}
						return
					}
					if res.Stopped || res.Text != "x" {
						t.Fatalf("result %+v, want x passing", res)
					}
					select {
					case got := <-called:
						if got != "x" {
							t.Fatalf("the next handler saw %q", got)
						}
					default:
						t.Fatal("the handler after a skipped one was not called")
					}
					if !strings.Contains(f.log.String(), tc.reason) {
						t.Fatalf("the failure is not logged with %q:\n%s", tc.reason, f.log)
					}
				})
			}
		}
	}
}

// A handler without the "conversation" grant gets the call with no text;
// one with it gets the text. Neither the text nor a plugin's reason is
// logged.
func TestNoTextWithoutConversation(t *testing.T) {
	const marker, reason = "MARKER-5d1c-text", "MARKER-9a7e-reason"
	f := newHookFixture(t, grant("reader", "message_sending", 10, true), grant("blind", "message_sending", 0, false))
	raws := map[string]string{}
	var mu sync.Mutex
	for _, id := range []string{"reader", "blind"} {
		f.connect(id, hookHandler(func(r control.HookRequest, raw json.RawMessage) (any, error) {
			mu.Lock()
			raws[id] = string(raw)
			mu.Unlock()
			if id == "blind" {
				return control.HookAnswer{Action: control.HookStop, Reason: reason}, nil
			}
			return control.HookAnswer{Action: control.HookContinue, Text: ptr(r.Text + " " + marker)}, nil
		}))
	}
	res := f.d.Run(context.Background(), control.HookCall{Point: "message_sending", Thread: 7, Scope: "s", Text: marker})
	if !res.Stopped || res.Failed || res.By != "blind" || res.Reason != reason {
		t.Fatalf("result %+v, want stopped by blind with its reason", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(raws["reader"], marker) {
		t.Fatalf("the plugin with conversation got %s", raws["reader"])
	}
	if strings.Contains(raws["blind"], "MARKER") || strings.Contains(raws["blind"], `"text"`) {
		t.Fatalf("the plugin without conversation got %s", raws["blind"])
	}
	if l := f.log.String(); strings.Contains(l, "MARKER") || !strings.Contains(l, "outcome=stop") {
		t.Fatalf("the log has text, or no outcome:\n%s", l)
	}
}

// recorder is a connection that records each request and blocks it until
// ctx ends.
type recorder struct {
	mu    sync.Mutex
	calls []string
	done  chan struct{}
}

func (r *recorder) Request(ctx context.Context, op string, args, result any) error {
	r.mu.Lock()
	r.calls = append(r.calls, op)
	r.mu.Unlock()
	if op != control.OpHook {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *recorder) Done() <-chan struct{} { return r.done }

// With no plugins, a point calls nothing and waits for nothing; and a
// plugin whose grant lacks the point or the scope is not called, whether
// it is connected or not.
func TestNoHandlerCallsNothing(t *testing.T) {
	call := control.HookCall{Point: "before_agent_run", Thread: 7, Scope: "s", Text: "x"}
	for _, d := range []*Dispatcher{
		NewDispatcher(nil, slog.New(slog.DiscardHandler)),
		func() *Dispatcher {
			other := grant("other-scope", "before_agent_run", 0, true)
			other.Scopes = []string{"t"}
			d := NewDispatcher([]Grant{other, grant("other-point", "message_sending", 0, true),
				grant("absent", "message_sending", 0, true)}, slog.New(slog.DiscardHandler))
			for _, id := range []string{"other-scope", "other-point"} {
				r := &recorder{done: make(chan struct{})}
				d.conns[id] = []Conn{r}
				t.Cleanup(func() {
					if len(r.calls) != 0 {
						t.Errorf("%s was sent %v", id, r.calls)
					}
				})
			}
			return d
		}(),
	} {
		began := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res := d.Run(ctx, call)
		cancel()
		if res != (control.HookResult{Text: "x"}) {
			t.Fatalf("result %+v", res)
		}
		if took := time.Since(began); took > time.Second {
			t.Fatalf("it waited %v", took)
		}
	}
}

// A plugin is told plugin_started with its grant on its first connection,
// not on a second one while the first is open, and again once it had
// none open; and plugin_stopping on the one it is called on.
func TestPluginStartedAndStopping(t *testing.T) {
	g := grant("p", "message_sending", 5, false)
	g.Hooks[0].OnFailure = Block
	g.Operations, g.Events = []string{"Threads"}, []control.EventKind{"turn_ended"}
	f := newHookFixture(t, g)
	got := make(chan string, 10)
	handler := func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		got <- op + " " + string(args)
		return nil, nil
	}
	next := func() string {
		select {
		case s := <-got:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("no request")
		}
		return ""
	}
	none := func() {
		select {
		case s := <-got:
			t.Fatalf("unexpected %s", s)
		case <-time.After(100 * time.Millisecond):
		}
	}
	c1, err := wire.DialTokenHandler(f.root, f.tokens["p"], handler)
	if err != nil {
		t.Fatal(err)
	}
	want := `plugin_started {"plugin":"p","scopes":["s"],"operations":["Threads"],"events":["turn_ended"],` +
		`"hooks":[{"point":"message_sending","priority":5,"timeout":5000,"on_failure":"block"}]}`
	if s := next(); s != want {
		t.Fatalf("got %s\nwant %s", s, want)
	}
	c2 := f.connect("p", handler)
	none()
	c1.Close()
	c2.Close()
	for deadline := time.Now().Add(5 * time.Second); f.d.conn("p") != nil; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the closed connections are still known")
		}
	}
	c3, err := wire.DialTokenHandler(f.root, f.tokens["p"], handler)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	if s := next(); s != want {
		t.Fatalf("after a new connection, got %s", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.d.Stopping(ctx, g)
	if s := next(); !strings.HasPrefix(s, `plugin_stopping {"plugin":"p","left":`) || strings.Contains(s, `"left":0`) {
		t.Fatalf("got %s", s)
	}
}

// Only a plugin's caller is marked as a plugin's: the owner's, and a
// caller that only says it is a plugin, are not.
func TestBindAs(t *testing.T) {
	as := BindAs(func(control.Caller) control.API { return stub{} })
	if b, ok := as(grant("p", "message_sending", 0, false).Caller()).(bound); !ok || b.id != "p" {
		t.Fatalf("a plugin's caller: %#v", b)
	}
	for _, c := range []control.Caller{{}, {From: "plugin:p"}, {From: "discord:p", Limit: &control.Limit{}}} {
		if _, ok := as(c).(bound); ok {
			t.Fatalf("%+v is marked", c)
		}
	}
	// Every hook point has its kind.
	for p := range points {
		if _, ok := kinds[p]; !ok {
			t.Errorf("point %s has no kind", p)
		}
	}
}
