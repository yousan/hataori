package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// supervisorRoot is a chat root with two scopes, c1 and c2, whose config
// lists the supervisor of c1 as the person sv1, allowed in c1 only. It
// returns what issues a supervisor's token, and what revokes one.
func supervisorRoot(t *testing.T) (*chatRoot, func() string, func(string) (string, error), func(string)) {
	t.Helper()
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["owner"] = "boss"
		cfg["people"] = []map[string]any{
			{"name": "boss", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
			{"name": "sv1", "ids": []string{supervisor.ID("c1")}},
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch", "sv1"}},
			{"key": "c2", "client": fakeSurface, "enabled": true, "people": []string{"dispatch"}},
		}
	})
	var issue func(string) (string, error)
	var revoke func(string)
	supervisorHook = func(i func(string) (string, error), rv func(string)) { issue, revoke = i, rv }
	t.Cleanup(func() { supervisorHook = nil })
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	return r, logged, issue, revoke
}

// refusedLine reports whether the daemon logged a refused call of op in
// scope.
func refusedLine(logged, op, scope string) bool {
	for _, l := range strings.Split(logged, "\n") {
		if strings.Contains(l, "control call refused") && strings.Contains(l, "op="+op) && strings.Contains(l, "scope="+scope) {
			return true
		}
	}
	return false
}

// threadEvents returns the data of the events of kind in thread id.
func threadEvents(t *testing.T, api control.API, id control.ThreadID, kind string) []map[string]any {
	t.Helper()
	d, err := api.Thread(context.Background(), id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range d.Events {
		if string(e.Kind) == kind {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}

// A supervisor's connection sees and acts in its own scope only: every
// call naming the other scope is refused and logged with the operation and
// the scope, and Answer and Stop are refused even in its own.
func TestSupervisorConnectionIsLimitedToItsScope(t *testing.T) {
	r, logged, issue, _ := supervisorRoot(t)
	ctx := context.Background()
	repo := bareRepo(t)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	a, err := owner.Start(ctx, control.StartRequest{Key: "k-a", Scope: "c1", Thread: "ta", Repo: repo, Input: control.Input{Text: "reply a"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := owner.Start(ctx, control.StartRequest{Key: "k-b", Scope: "c2", Thread: "tb", Repo: repo, Input: control.Input{Text: "reply b"}})
	if err != nil {
		t.Fatal(err)
	}

	token, err := issue("c1")
	if err != nil {
		t.Fatal(err)
	}
	sv, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer sv.Close()

	ts, err := sv.Threads(ctx, control.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].ID != a {
		t.Fatalf("the supervisor of c1 lists %+v, want only %v", ts, a)
	}
	if _, err := sv.Threads(ctx, control.Filter{Scope: "c2"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Threads in c2: %v, want forbidden", err)
	}
	// A thread of another scope is answered as one that does not exist
	// (#479).
	if _, err := sv.Thread(ctx, b, 100, 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Thread in c2: %v, want not found", err)
	}
	if err := sv.Send(ctx, b, "s-b", control.Input{Text: "reply no"}, control.Refuse); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Send in c2: %v, want not found", err)
	}
	if _, err := sv.Start(ctx, control.StartRequest{Key: "k-c", Scope: "c2", Thread: "tc", Repo: repo, Input: control.Input{Text: "reply no"}}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Start in c2: %v, want forbidden", err)
	}
	if err := sv.Stop(ctx, a, 0); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("Stop in c1: %v, want forbidden", err)
	}
	if _, err := sv.SignInLink(ctx, ""); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("SignInLink: %v, want forbidden", err)
	}
	eventually(t, "the refusals logged", 5*time.Second, func() bool {
		l := logged()
		return refusedLine(l, "Threads", "c2") && refusedLine(l, "Thread", "c2") && refusedLine(l, "Send", "c2") &&
			refusedLine(l, "Start", "c2") && refusedLine(l, "Stop", "c1")
	})
	if th, _ := owner.Threads(ctx, control.Filter{Scope: "c2"}); len(th) != 1 {
		t.Fatalf("c2 has %d threads, want the one the owner started", len(th))
	}

	// A token is per supervisor: c1's does not work for c2, and a token
	// for c2 does not work for c1.
	t2, err := issue("c2")
	if err != nil {
		t.Fatal(err)
	}
	sv2, err := wire.DialToken(r.root, t2)
	if err != nil {
		t.Fatal(err)
	}
	defer sv2.Close()
	if ts, err := sv2.Threads(ctx, control.Filter{}); err != nil || len(ts) != 1 || ts[0].ID != b {
		t.Fatalf("the supervisor of c2 lists %+v, %v; want only %v", ts, err, b)
	}
	if _, err := sv2.Thread(ctx, a, 100, 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("c2's token reads c1: %v, want not found", err)
	}
	// c2's supervisor is no person of the config, so it may read its
	// scope but give no input there.
	if err := sv2.Send(ctx, b, "s-b2", control.Input{Text: "reply no"}, control.Refuse); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("a supervisor no person lists sends: %v, want forbidden", err)
	}
}

// An unknown token is refused and logged without the token; a revoked one
// stops working and its connections are closed; a connection with no token
// is still the owner.
func TestUnknownAndRevokedTokens(t *testing.T) {
	r, logged, issue, revoke := supervisorRoot(t)
	const bogus = "bogus-token-Zq81xk"
	if _, err := wire.DialToken(r.root, bogus); err == nil || !strings.Contains(err.Error(), "unknown token") {
		t.Fatalf("an unknown token: %v, want refused", err)
	}
	eventually(t, "the refusal logged", 5*time.Second, func() bool { return strings.Contains(logged(), "unknown token") })
	if strings.Contains(logged(), bogus) {
		t.Fatalf("the log has the token: %s", logged())
	}

	token, err := issue("c1")
	if err != nil {
		t.Fatal(err)
	}
	sv, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer sv.Close()
	ctx := context.Background()
	if _, err := sv.Threads(ctx, control.Filter{}); err != nil {
		t.Fatal(err)
	}
	revoke(token)
	if _, err := sv.Threads(ctx, control.Filter{}); err == nil {
		// The first call may still have been on its way; the client then
		// connects again, with the revoked token.
		if _, err := sv.Threads(ctx, control.Filter{}); err == nil {
			t.Fatal("a revoked token still works")
		}
	}
	if _, err := wire.DialToken(r.root, token); err == nil {
		t.Fatal("a revoked token connects")
	}
	if strings.Contains(logged(), token) {
		t.Fatal("the log has a token")
	}

	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Threads(ctx, control.Filter{Scope: "c2"}); err != nil {
		t.Fatalf("the owner, with no token: %v", err)
	}
}

// Sending to a busy thread is refused with "busy", whatever the
// supervisor's connection asks for: the turn is not interrupted and
// nothing is queued. Its input reaches the agent with the mark, and the
// input event names its person. (Its start, marked the same way, is
// TestSupervisorStartsOnlyInItsRepos: this scope lists no repos.)
func TestSupervisorSendIsRefusedWhenBusyAndMarked(t *testing.T) {
	r, logged, issue, _ := supervisorRoot(t)
	ctx := context.Background()
	token, err := issue("c1")
	if err != nil {
		t.Fatal(err)
	}
	sv, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer sv.Close()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	// The owner starts a thread, and the supervisor's send runs its next
	// turn.
	id, err := owner.Start(ctx, control.StartRequest{Key: "o-1", Scope: "c1", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply ready"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first turn to end", 15*time.Second, func() bool {
		d, err := sv.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Idle && len(threadEvents(t, sv, id, "turn_ended")) == 1
	})
	if err := sv.Send(ctx, id, "sv-1", control.Input{Text: "heard\ntool t 2s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn to run", 15*time.Second, func() bool {
		d, err := sv.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Running
	})
	for _, busy := range []control.Busy{control.Refuse, control.Deliver, control.Interrupt} {
		err := sv.Send(ctx, id, "busy-"+string(rune('0'+busy)), control.Input{Text: "reply no"}, busy)
		if !errors.Is(err, core.ErrBusy) {
			t.Fatalf("Send with busy %d to a running thread: %v, want busy", busy, err)
		}
	}
	eventually(t, "the turn to end", 15*time.Second, func() bool {
		return len(threadEvents(t, sv, id, "turn_ended")) == 2
	})
	ended := threadEvents(t, sv, id, "turn_ended")[1]
	if ended["outcome"] != "completion" {
		t.Fatalf("the turn ended %v, want completion: it was interrupted", ended)
	}
	time.Sleep(300 * time.Millisecond) // a queued input would start a turn now
	if n := len(threadEvents(t, sv, id, "turn_started")); n != 2 {
		t.Fatalf("%d turns started, want 2: a refused send was queued", n)
	}
	inputs := threadEvents(t, sv, id, "input")
	if len(inputs) != 2 {
		t.Fatalf("input events %v, want the first input and the supervisor's only", inputs)
	}
	in := inputs[1]
	if in["principal"] != "sv1" || in["from"] != supervisor.ID("c1") || !strings.HasPrefix(in["text"].(string), supervisor.Mark+"\n") {
		t.Fatalf("the input event %v, want sv1's, from %s, marked", in, supervisor.ID("c1"))
	}
	texts := threadEvents(t, sv, id, "text")
	if len(texts) == 0 || texts[len(texts)-1]["text"] != "heard: "+supervisor.Mark {
		t.Fatalf("the agent's text %v, want it to have heard the mark first", texts)
	}
	d, err := sv.Thread(ctx, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !refusedLine(logged(), "Send", "c1") {
		t.Fatal("the Deliver and Interrupt sends were not logged")
	}

	// Through hataori mcp: the same busy thread is refused with "busy" as
	// a tool error, and an idle one takes the message, marked.
	if err := sv.Send(ctx, id, "next", control.Input{Text: "tool t 2s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn to run", 15*time.Second, func() bool {
		d, err := sv.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Running
	})
	m := &supervisor.MCP{Dial: func() (control.API, error) { return wire.DialToken(r.root, token) }, Scope: "c1"}
	out := mcpCall(t, m, supervisor.ToolSend, map[string]any{"number": int64(d.ID), "text": "reply no"})
	if !out.IsError || !strings.HasPrefix(out.Text, "busy") {
		t.Fatalf("send_to_thread to a busy thread: %+v, want a busy tool error", out)
	}
	eventually(t, "the turn to end", 15*time.Second, func() bool {
		return len(threadEvents(t, sv, id, "turn_ended")) == 3
	})
	out = mcpCall(t, m, supervisor.ToolSend, map[string]any{"number": int64(d.ID), "text": "heard"})
	if out.IsError {
		t.Fatalf("send_to_thread to an idle thread: %+v", out)
	}
	eventually(t, "the marked reply", 15*time.Second, func() bool {
		texts := threadEvents(t, sv, id, "text")
		return len(texts) > 0 && texts[len(texts)-1]["text"] == "heard: "+supervisor.Mark
	})
}

// toolAnswer is the text of a tools/call result, and whether it is an
// error.
type toolAnswer struct {
	Text    string
	IsError bool
}

// mcpCall makes one tools/call of m, as an MCP client would.
func mcpCall(t *testing.T, m *supervisor.MCP, name string, args map[string]any) toolAnswer {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	var out bytes.Buffer
	if err := m.Serve(context.Background(), bytes.NewReader(append(req, '\n')), &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || len(resp.Result.Content) != 1 {
		t.Fatalf("tools/call answered %s (%v)", out.String(), err)
	}
	return toolAnswer{resp.Result.Content[0].Text, resp.Result.IsError}
}

// A supervisor's send that answers a worker's open question reaches the
// agent as given, without the mark, and the answer event names its person.
func TestSupervisorAnswerIsNotMarked(t *testing.T) {
	r, _, issue, _ := supervisorRoot(t)
	ctx := context.Background()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	id, err := owner.Start(ctx, control.StartRequest{Key: "k-q", Scope: "c1", Thread: "tq", Repo: bareRepo(t), Input: control.Input{Text: "ask Which colour?"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the question", 15*time.Second, func() bool {
		d, err := owner.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Waiting
	})
	token, err := issue("c1")
	if err != nil {
		t.Fatal(err)
	}
	sv, err := wire.DialToken(r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer sv.Close()
	if err := sv.Send(ctx, id, "a-1", control.Input{Text: "blue"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer's reply", 15*time.Second, func() bool {
		texts := threadEvents(t, sv, id, "text")
		return len(texts) > 0 && strings.HasPrefix(texts[len(texts)-1]["text"].(string), "answer: ")
	})
	texts := threadEvents(t, sv, id, "text")
	if got := texts[len(texts)-1]["text"]; got != "answer: blue" {
		t.Fatalf("the agent got the answer %q, want it unmarked", got)
	}
	answers := threadEvents(t, sv, id, "answer")
	if len(answers) != 1 || answers[0]["text"] != "blue" || answers[0]["principal"] != "sv1" || answers[0]["from"] != supervisor.ID("c1") {
		t.Fatalf("answer events %v", answers)
	}
}
