package wire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

type nopRunner struct{}

func (nopRunner) Start(context.Context, core.Launch) error                   { return nil }
func (nopRunner) Deliver(context.Context, int64, int64, control.Input) error { return nil }
func (nopRunner) Interrupt(context.Context, int64) error                     { return nil }
func (nopRunner) Answer(context.Context, int64, int64, string, control.Answer) error {
	return nil
}
func (nopRunner) Resume(context.Context, int64, control.Principal, string, control.Input) error {
	return nil
}

type dirs struct{}

func (dirs) Make(context.Context, control.ThreadID, string, func(core.Placement) error) (string, error) {
	return "dir", nil
}

func (dirs) Adopt(context.Context, control.ThreadID, core.Placement) (string, bool, error) {
	return "", false, nil
}

func (dirs) State(context.Context, string) (control.WorkspaceState, error) {
	return control.WorkspaceState{Branch: "main"}, nil
}

const all = control.OpRead | control.OpStart | control.OpSend | control.OpAnswer | control.OpStop

// newCore returns a core in a new root, and the owner's API in process.
func newCore(t *testing.T, root string) (*core.Core, control.API) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &core.Core{
		People: people.New(nil, "owner", nil),
		Store:  st, Machine: &session.Machine{Store: st},
		Scopes:     map[string]core.Scope{"s": {Enabled: true, Owner: "chat"}, "off": {Owner: "chat"}},
		Workspaces: dirs{}, Runner: nopRunner{}, Agent: "fake",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return c, c.As(control.Caller{Principal: "owner", Scopes: []string{"s", "off"}, May: all})
}

func serve(t *testing.T, root string, api control.API) *Server {
	t.Helper()
	srv, err := Listen(root, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(cancel)
	return srv
}

// cases runs the same calls against api and returns what each gave.
func cases(t *testing.T, c *core.Core, api control.API) []string {
	ctx := context.Background()
	var got []string
	note := func(name string, v any, err error) {
		s := name + ": "
		switch {
		case errors.Is(err, core.ErrDisabled):
			s += "disabled"
		case errors.Is(err, core.ErrBusy):
			s += "busy"
		case errors.Is(err, core.ErrNotFound):
			s += "not found"
		case err != nil:
			s += "error " + err.Error()
		default:
			s += strings.ReplaceAll(strings.TrimSpace(strings.Join(strings.Fields(sprint(v)), " ")), "\n", " ")
		}
		got = append(got, s)
	}
	id, err := api.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Input: control.Input{Text: "hello"}})
	note("start", id, err)
	id2, err := api.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "other"})
	note("start again", id2, err)
	_, err = api.Start(ctx, control.StartRequest{Key: "k2", Scope: "off", Thread: "t2"})
	note("start disabled", nil, err)

	var sid int64
	c.Store.DB.QueryRow(`SELECT id FROM sessions`).Scan(&sid)
	c.Machine.Apply(ctx, sid, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 1, Kind: agent.SessionReady}})
	note("send", nil, api.Send(ctx, id, "m1", control.Input{Text: "go"}, control.Refuse))
	note("send busy", nil, api.Send(ctx, id, "m2", control.Input{Text: "go"}, control.Refuse))
	c.Machine.Apply(ctx, sid, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 2, Kind: agent.Text, Text: "answer text"}})
	note("answer unknown", nil, api.Answer(ctx, "nope", control.Answer{}))
	note("stop", nil, api.Stop(ctx, id))

	ths, err := api.Threads(ctx, control.Filter{})
	var states []string
	for _, th := range ths {
		states = append(states, th.ID.Key+"="+string(th.State)+"/"+th.Title)
	}
	note("threads", states, err)
	d, err := api.Thread(ctx, id, 6, 0)
	note("thread", []string{string(d.State), d.Recent}, err)
	ws, err := api.Workspace(ctx, id)
	note("workspace", []string{ws.Branch}, err)
	_, err = api.Workspace(ctx, control.ThreadID{Scope: "s", Key: "none"})
	note("workspace of no thread", nil, err)

	evctx, cancel := context.WithCancel(ctx)
	defer cancel()
	evs, err := api.Events(evctx, control.Filter{Scope: "s"}, 0)
	var kinds []string
	if err == nil {
		for e := range evs {
			kinds = append(kinds, string(e.Kind))
			if e.Kind == "text" {
				break
			}
		}
	}
	note("events", kinds, err)
	return got
}

func sprint(v any) string {
	switch v := v.(type) {
	case nil:
		return "ok"
	case []string:
		return strings.Join(v, ",")
	case control.ThreadID:
		return v.Scope + "/" + v.Key
	}
	return "?"
}

func TestSameResultsInProcessAndOverSocket(t *testing.T) {
	c1, local := newCore(t, t.TempDir())
	want := cases(t, c1, local)

	root := t.TempDir()
	c2, owner := newCore(t, root)
	serve(t, root, owner)
	client, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got := cases(t, c2, client)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("over the socket:\n%s\nin process:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestOtherUserIsRefused(t *testing.T) {
	root := t.TempDir()
	_, owner := newCore(t, root)
	srv := serve(t, root, owner)
	st, err := os.Stat(srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode %o is open to others", st.Mode().Perm())
	}
	srv.uid = os.Getuid() + 1 // pretend the daemon runs as someone else
	if _, err := Dial(root); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Dial from another user: %v", err)
	}
}

func TestEventsResumeAfterReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	c, owner := newCore(t, root)
	srv := serve(t, root, owner)
	client, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	appendN := func(n int) {
		c.Store.Update(ctx, func(tx *store.Tx) error {
			for range n {
				tx.Append(ctx, control.Event{Thread: control.ThreadID{Scope: "s", Key: "k"}, Kind: "usage", Data: []byte(`{"session":1}`)})
			}
			return nil
		})
	}
	appendN(5)
	evs, err := client.Events(ctx, control.Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	var got []uint64
	read := func(n int) {
		for want := len(got) + n; len(got) < want; {
			select {
			case e, ok := <-evs:
				if !ok {
					t.Fatal("stream ended")
				}
				if e.Kind == control.CaughtUp {
					continue // a mark, not an event; it does not count
				}
				got = append(got, e.Pos)
			case <-time.After(5 * time.Second):
				t.Fatalf("no event after %v", got)
			}
		}
	}
	read(2)
	srv.DropConns() // the connection breaks in the middle of the stream
	appendN(5)
	read(6)
	for i, p := range got {
		if p != uint64(3+i) {
			t.Fatalf("positions %v: gap or repeat", got)
		}
	}
}

func TestVersionMismatch(t *testing.T) {
	root := t.TempDir()
	_, owner := newCore(t, root)
	srv := serve(t, root, owner)
	_, err := dialPath(srv.Path(), "0.9")
	if err == nil || !strings.Contains(err.Error(), "0.9") || !strings.Contains(err.Error(), Version) {
		t.Fatalf("Dial with another version: %v", err)
	}
}

func TestDeepRootUsesShortSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := filepath.Join(t.TempDir(), strings.Repeat("deep/", 30))
	_, owner := newCore(t, root)
	srv := serve(t, root, owner)
	if len(srv.Path()) > MaxSocketPath || strings.HasPrefix(srv.Path(), root) {
		t.Fatalf("socket path %q", srv.Path())
	}
	client, err := Dial(root)
	if err != nil {
		t.Fatalf("Dial with a deep root: %v", err)
	}
	defer client.Close()
	if _, err := client.Threads(context.Background(), control.Filter{}); err != nil {
		t.Fatal(err)
	}
}
