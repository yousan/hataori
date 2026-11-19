package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
)

// Every operation that names a thread, by its number, by one of its
// requests or by one of its links, answers a caller for a thread of a scope
// that is not one of the caller's exactly as for one that does not exist:
// the same error, which names no scope outside the caller's. The log line
// names the scope. Nothing changes (#479).
func TestOperationOnAThreadOfAnotherScope(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	const hiddenScope = "hidden-scope"
	f.core.Scopes[hiddenScope] = Scope{Enabled: true, Owner: "chat"}
	f.core.People = people.New([]people.Person{{Name: "sv", IDs: []string{"supervisor:" + hiddenScope}}},
		"owner", map[string][]string{hiddenScope: {"sv"}})
	inHidden := f.core.As(control.Caller{Principal: "owner", Scopes: []string{hiddenScope}, May: all})
	running := func(key string) (control.ThreadID, int64) {
		id, err := inHidden.Start(ctx, control.StartRequest{Key: key, Scope: hiddenScope, Thread: key, Input: control.Input{Text: "go"}})
		if err != nil {
			t.Fatal(err)
		}
		sid, _, err := f.core.current(ctx, f.core.Store.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		f.agentEvent(t, sid, 1, agent.Event{Kind: agent.SessionReady})
		if err := inHidden.Send(ctx, id, key+"-first", control.Input{Text: "go"}, control.Refuse); err != nil {
			t.Fatal(err)
		}
		return id, sid
	}
	// The hidden scope's thread runs, with a question open, an approval of
	// a supervisor of its scope open, and a link in a channel of its
	// show_in.
	hidden, sid := running("t1")
	asksIn, _ := running("approvals")
	approval, err := f.core.As(control.Caller{From: "supervisor:" + hiddenScope, Scopes: []string{hiddenScope}, May: control.OpAsk, AsksIn: asksIn}).
		AskInterrupt(ctx, hidden, "ask-1", "urgent")
	if err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})
	link := control.LinkRef{Client: "chat", Channel: "elsewhere", Thread: "c9"}
	if _, err := f.core.Store.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, created_at) VALUES (?, ?, ?, ?, 0)`,
		int64(hidden), link.Client, link.Channel, link.Thread); err != nil {
		t.Fatal(err)
	}
	missing := hidden + 1000
	noLink := link
	noLink.Thread = "c-none"

	// Each operation, called once for the hidden thread (or its request or
	// link) and once for one that does not exist; ids are what the caller
	// gave, left out when the two answers are compared.
	type target struct {
		thread  control.ThreadID
		request control.RequestID
		link    control.LinkRef
	}
	targets := [2]target{
		{hidden, session.RequestHandle(sid, "r1"), link},
		{missing, session.RequestHandle(sid+1000, "r1"), noLink},
	}
	ops := map[string]func(a control.API, x target) error{
		"Thread":       func(a control.API, x target) error { _, err := a.Thread(ctx, x.thread, 10, 5); return err },
		"ThreadEvents": func(a control.API, x target) error { _, err := a.ThreadEvents(ctx, x.thread, 0, 5); return err },
		"Workspace":    func(a control.API, x target) error { _, err := a.Workspace(ctx, x.thread); return err },
		"Terminal":     func(a control.API, x target) error { _, err := a.Terminal(ctx, x.thread, "", 0); return err },
		"AttachTerminal": func(a control.API, x target) error {
			_, err := a.AttachTerminal(ctx, x.thread)
			return err
		},
		"Send": func(a control.API, x target) error {
			return a.Send(ctx, x.thread, "k9", control.Input{Text: "x"}, control.Deliver)
		},
		"AskInterrupt": func(a control.API, x target) error { _, err := a.AskInterrupt(ctx, x.thread, "k", "r"); return err },
		"Stop":         func(a control.API, x target) error { return a.Stop(ctx, x.thread, 0) },
		"Close":        func(a control.API, x target) error { return a.CloseThread(ctx, x.thread) },
		"Remove":       func(a control.API, x target) error { return a.RemoveThread(ctx, x.thread) },
		"Link": func(a control.API, x target) error {
			_, err := a.Link(ctx, x.thread, control.LinkRef{Client: "chat", Channel: "s", Thread: "c2"})
			return err
		},
		"Answer a question": func(a control.API, x target) error {
			return a.Answer(ctx, x.request, control.Answer{Text: "x"})
		},
		"Answer an approval": func(a control.API, x target) error {
			req := approval
			if x.thread == missing {
				req = "approval-00000000000000000000000000000000"
			}
			return a.Answer(ctx, req, control.Answer{Allow: true})
		},
		"SetName": func(a control.API, x target) error { return a.SetName(ctx, x.link, "a name") },
	}
	// ids are the caller's own input as the messages quote it.
	ids := func(op string, x target) []string {
		switch op {
		case "Answer a question":
			return []string{strconv.Quote(string(x.request))}
		case "Answer an approval":
			if x.thread == missing {
				return []string{strconv.Quote("approval-00000000000000000000000000000000")}
			}
			return []string{strconv.Quote(string(approval))}
		case "SetName":
			return []string{"/" + x.link.Thread + " "}
		}
		return []string{"thread " + strconv.FormatInt(int64(x.thread), 10) + ":"}
	}
	answer := func(op string, a control.API, x target) string {
		err := ops[op](a, x)
		code := "nil"
		switch {
		case errors.Is(err, ErrNotFound):
			code = "not_found"
		case errors.Is(err, ErrForbidden):
			code = "forbidden"
		case err != nil:
			code = "error"
		}
		msg := fmt.Sprint(err)
		for _, id := range ids(op, x) {
			msg = strings.ReplaceAll(msg, id, "<id>")
		}
		return code + ": " + msg
	}

	may := all | control.OpClose | control.OpRemove | control.OpAttach | control.OpName
	callers := map[string]control.Caller{
		// One of another scope, allowed every operation there.
		"another scope's": {Principal: "owner", Scopes: []string{"s"}, May: may},
		// The same, limited to no operation, as a plugin with no grant.
		"a limited": {Principal: "owner", Scopes: []string{"s"}, May: may, Limit: &control.Limit{}},
	}
	before := func() string {
		return fmt.Sprint(count(t, f.core.Store, "events"), count(t, f.core.Store, "threads"), count(t, f.core.Store, "links"),
			count(t, f.core.Store, "names"), f.runner.Calls())
	}
	was := before()
	for cname, c := range callers {
		a := f.core.As(c)
		for op := range ops {
			what := fmt.Sprintf("%s by %s caller", op, cname)
			f.log.Reset()
			got := answer(op, a, targets[0])
			logged := f.log.String()
			want := answer(op, a, targets[1])
			if got != want {
				t.Errorf("%s: the hidden scope's: %s; none: %s", what, got, want)
			}
			if strings.Contains(got, hiddenScope) {
				t.Errorf("%s: the answer names the hidden scope: %s", what, got)
			}
			if !strings.Contains(logged, hiddenScope) {
				t.Errorf("%s: the log does not name the scope: %q", what, logged)
			}
		}
	}
	if now := before(); now != was {
		t.Fatalf("the calls changed something: %s, was %s", now, was)
	}
	// The ids are right: the hidden scope's own caller reaches the thread.
	if _, err := inHidden.Thread(ctx, hidden, 10, 5); err != nil {
		t.Fatal(err)
	}
}
