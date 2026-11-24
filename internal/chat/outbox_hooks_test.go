package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// rewriter is Hooks that rewrites every text at message_sending, and
// counts its calls.
type rewriter struct {
	mu    sync.Mutex
	calls []control.HookCall
}

func (h *rewriter) Run(_ context.Context, c control.HookCall) control.HookResult {
	h.mu.Lock()
	h.calls = append(h.calls, c)
	h.mu.Unlock()
	return control.HookResult{Text: "rewritten: " + c.Text}
}

// With a rewrite, every link shows the rewritten text: the same part of an
// event in two links of one thread is hooked once, with the thread's
// number and scope, and both posts carry what the handler made of it.
func TestARewriteIsTheSameInEveryLink(t *testing.T) {
	a, b := surface.Conversation{Channel: "c1", Thread: "a"}, surface.Conversation{Channel: "c2", Thread: "b"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a, b}})
	d := fake.New()
	h := &rewriter{}
	o := &Outbox{Store: st, Client: "f", Driver: d, Hooks: h}
	if err := o.Write(ctx, row(5, 0, a, "hello"), row(5, 0, b, "hello")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "both posts", func() bool { return len(postsIn(d, a)) == 1 && len(postsIn(d, b)) == 1 })
	if got := append(postsIn(d, a), postsIn(d, b)...); !slices.Equal(got, []string{"rewritten: hello", "rewritten: hello"}) {
		t.Fatalf("posts %q", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 1 || h.calls[0].Thread != 7 || h.calls[0].Scope != "c1" || h.calls[0].Point != PointMessageSending {
		t.Fatalf("calls %+v", h.calls)
	}
}

// script is Hooks that answers each text as answers says, and records the
// texts it was called with.
type script struct {
	mu      sync.Mutex
	texts   []string
	calls   []control.HookCall
	answers map[string]control.HookResult
}

func (h *script) Run(_ context.Context, c control.HookCall) control.HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.texts = append(h.texts, c.Text)
	h.calls = append(h.calls, c)
	if r, ok := h.answers[c.Text]; ok {
		return r
	}
	return control.HookResult{Text: c.Text}
}

func (h *script) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.texts)
}

// newThread opens a store at root with thread 7 in scope c1, linked in
// each conversation of links, by client.
func newThread(t *testing.T, root string, links map[string][]surface.Conversation) *store.Store {
	t.Helper()
	st := openStore(t, root)
	if _, err := st.DB.Exec(`INSERT INTO threads (id, scope, key, title, repo, started_by, state, created_at, updated_at)
		VALUES (7, 'c1', 'a', '', '', 'x', 'idle', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	for client, convs := range links {
		for _, c := range convs {
			if _, err := st.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, made_by, created_at) VALUES (7, ?, ?, ?, 'x', 0)`,
				client, c.Channel, c.Thread); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st
}

// An edit is hooked when it changes the text the post shows now, as the
// post or its last edit left it, and not when it keeps it; a cancelled edit
// leaves the post as it was.
func TestEditsAreHookedWhenTheyChangeTheText(t *testing.T) {
	a := surface.Conversation{Channel: "c1", Thread: "a"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a}})
	d := fake.New()
	h := &script{answers: map[string]control.HookResult{
		"B":      {Text: "B!"},
		"SECRET": {Stopped: true, By: "p", Reason: "no"},
	}}
	o := &Outbox{Store: st, Client: "f", Driver: d, Hooks: h}
	edit := func(event uint64, text string) Row {
		return Row{Event: event, Conv: a, Op: Edit, Target: &RowKey{Event: 1}, Message: surface.Message{Text: text}}
	}
	run(t, o)
	// One at a time: a later edit of the same post drops the ones queued
	// before it.
	for _, r := range []Row{row(1, 0, a, "A"), edit(2, "B"), edit(3, "B!"), edit(4, "SECRET"), edit(5, "A")} {
		if err := o.Write(ctx, r); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the row done", func() bool {
			var n int
			st.DB.QueryRow(`SELECT count(*) FROM chat_outbox WHERE event = ? AND state != 'pending'`, r.Event).Scan(&n)
			return n == 1
		})
	}
	var edits []string
	for _, c := range d.Calls(a) {
		if c.Op == fake.OpEdit {
			edits = append(edits, c.Message.Text)
		}
	}
	if !slices.Equal(edits, []string{"B!", "B!", "A"}) {
		t.Fatalf("edits %q", edits)
	}
	if got := h.got(); !slices.Equal(got, []string{"A", "B", "SECRET", "A"}) {
		t.Fatalf("hooked %q", got)
	}
}

// The copies of one part on two front ends are hooked once, also when
// they have no text.
func TestCopiesOnTwoFrontEndsAreHookedOnce(t *testing.T) {
	a, b := surface.Conversation{Channel: "c1", Thread: "a"}, surface.Conversation{Channel: "c9", Thread: "b"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a}, "g": {b}})
	h := &slow{}
	df, dg := fake.New(), fake.New()
	of := &Outbox{Store: st, Client: "f", Driver: df, Hooks: h}
	og := &Outbox{Store: st, Client: "g", Driver: dg, Hooks: h}
	for _, text := range []string{"hello", ""} {
		event := uint64(len(text) + 1)
		if err := of.Write(ctx, row(event, 0, a, text)); err != nil {
			t.Fatal(err)
		}
		if err := og.Write(ctx, row(event, 0, b, text)); err != nil {
			t.Fatal(err)
		}
	}
	run(t, of)
	run(t, og)
	waitFor(t, "the posts", func() bool { return len(postsIn(df, a)) == 2 && len(postsIn(dg, b)) == 2 })
	if got := h.got(); len(got) != 2 {
		t.Fatalf("hooked %q, want each part once", got)
	}
}

// slow is script whose calls take a while, so that two calls of one part
// that are not held one after the other overlap, and both miss the other's
// result.
type slow struct{ script }

func (h *slow) Run(ctx context.Context, c control.HookCall) control.HookResult {
	r := h.script.Run(ctx, c)
	time.Sleep(200 * time.Millisecond)
	return r
}

// A row with no link in a chat thread of a scope's channel is hooked in
// that scope, with no thread; one in no scope's channel is not hooked.
func TestUnlinkedRowsOfAScope(t *testing.T) {
	st := openStore(t, t.TempDir())
	d := fake.New()
	h := &script{}
	o := &Outbox{Store: st, Client: "f", Driver: d, Hooks: h, Scopes: []string{"c1"}}
	x, dm := surface.Conversation{Channel: "c1", Thread: "x"}, surface.Conversation{Channel: "dm-1"}
	if err := o.Write(ctx, row(0, 1, x, "status"), row(0, 2, dm, "direct")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "the posts", func() bool { return len(postsIn(d, x)) == 1 && len(postsIn(d, dm)) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 1 || h.calls[0].Text != "status" || h.calls[0].Scope != "c1" || h.calls[0].Thread != 0 {
		t.Fatalf("calls %+v", h.calls)
	}
}

// noEmpty is the fake driver, except that it refuses a post with nothing to
// post before it sends anything, as a driver of a service does.
type noEmpty struct{ *fake.Driver }

func (d noEmpty) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	if m.Text == "" && len(m.Files) == 0 {
		return surface.MessageRef{}, fmt.Errorf("nothing to post: %w", surface.ErrRefused)
	}
	return d.Driver.Post(ctx, conv, key, m)
}

// A message_sending handler that answers continue with an empty text must
// not stop the thread's chat output: the rows behind that one are posted.
func TestAnEmptyRewriteDoesNotHoldBackTheRowsBehindIt(t *testing.T) {
	a := surface.Conversation{Channel: "c1", Thread: "a"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a}})
	d := fake.New()
	h := &script{answers: map[string]control.HookResult{"first": {Text: ""}}}
	o := &Outbox{Store: st, Client: "f", Driver: noEmpty{d}, Hooks: h, Retry: 20 * time.Millisecond}
	if err := o.Write(ctx, row(5, 0, a, "first"), row(6, 0, a, "second")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !slices.Contains(postsIn(d, a), "second"); {
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Contains(postsIn(d, a), "second") {
		var state, hook, behind string
		var failures int
		var text any
		st.DB.QueryRow(`SELECT state, hook, hook_text, failures FROM chat_outbox WHERE event = 5`).Scan(&state, &hook, &text, &failures)
		st.DB.QueryRow(`SELECT state FROM chat_outbox WHERE event = 6`).Scan(&behind)
		t.Fatalf("the row behind the rewritten one was not posted in 3s: posts %q; the rewritten row: state %q, hook %q, hook_text %q, %d failed posts; the row behind it: state %q; the handler was called %d time(s)",
			postsIn(d, a), state, hook, text, failures, behind, len(h.got()))
	}
}

// The outbox tells the hooks whether a post has files, which make it
// something to post without a text.
func TestHooksAreToldOfFiles(t *testing.T) {
	a := surface.Conversation{Channel: "c1", Thread: "a"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a}})
	d := fake.New()
	h := &script{}
	o := &Outbox{Store: st, Client: "f", Driver: d, Hooks: h}
	with := row(6, 0, a, "with")
	with.Message.Files = []surface.File{{Name: "f", Path: "/f"}}
	if err := o.Write(ctx, row(5, 0, a, "without"), with); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "both posts", func() bool { return len(postsIn(d, a)) == 2 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 2 || h.calls[0].Files || !h.calls[1].Files {
		t.Fatalf("calls %+v", h.calls)
	}
}

// A reply the service refuses for good is given up after one try: the rows
// behind it are posted, a reply to it without the reply, and the log and
// one post_refused event say what happened.
func TestARefusedReplyIsGivenUp(t *testing.T) {
	a := surface.Conversation{Channel: "c1", Thread: "a"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a}})
	d := fake.New()
	d.Fail(fake.OpPost, 1, fmt.Errorf("post: no permission: %w", surface.ErrRefused))
	var log bytes.Buffer
	o := &Outbox{Store: st, Client: "f", Driver: d, Retry: 20 * time.Millisecond, Log: slog.New(slog.NewTextHandler(&log, nil))}
	reply := row(6, 0, a, "second")
	reply.ReplyToRow = &RowKey{Event: 5}
	if err := o.Write(ctx, row(5, 0, a, "first"), reply); err != nil {
		t.Fatal(err)
	}
	stop := run(t, o)
	waitFor(t, "the row behind it", func() bool { return slices.Contains(postsIn(d, a), "second") })
	waitFor(t, "nothing pending", func() bool {
		var n int
		st.DB.QueryRow(`SELECT count(*) FROM chat_outbox WHERE state = 'pending'`).Scan(&n)
		return n == 0
	})
	stop()
	var state string
	var failures int
	st.DB.QueryRow(`SELECT state, failures FROM chat_outbox WHERE event = 5`).Scan(&state, &failures)
	if state != "dropped" || failures != 1 {
		t.Fatalf("the refused row: %s, %d failed posts", state, failures)
	}
	rows, err := st.DB.Query(`SELECT thread_id, scope, data FROM events WHERE kind = 'post_refused'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var thread int64
		var scope string
		var data []byte
		if err := rows.Scan(&thread, &scope, &data); err != nil {
			t.Fatal(err)
		}
		var p control.RefusedPost
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		if thread != 7 || scope != "c1" || p.Event != 5 || p.Link != (control.LinkRef{Client: "f", Channel: "c1", Thread: "a"}) || !strings.Contains(p.Reason, "no permission") {
			t.Fatalf("post_refused %s of thread %d in %s", data, thread, scope)
		}
	}
	if n != 1 {
		t.Fatalf("%d post_refused events", n)
	}
	if got := strings.Count(log.String(), "refused a post"); got != 1 || !strings.Contains(log.String(), "no permission") {
		t.Fatalf("%d log lines of the refused post:\n%s", got, log.String())
	}
}

// A reply whose post fails with any other error is tried again, and holds
// back the rows behind it until it is posted.
func TestAReplyThatFailsForNowIsTriedAgain(t *testing.T) {
	a := surface.Conversation{Channel: "c1", Thread: "a"}
	st := newThread(t, t.TempDir(), map[string][]surface.Conversation{"f": {a}})
	d := fake.New()
	d.Fail(fake.OpPost, 5, errors.New("no answer"))
	o := &Outbox{Store: st, Client: "f", Driver: d, Retry: 20 * time.Millisecond}
	if err := o.Write(ctx, row(5, 0, a, "first"), row(6, 0, a, "second")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "both posts", func() bool { return slices.Contains(postsIn(d, a), "second") })
	if got := postsIn(d, a); !slices.Equal(got[len(got)-2:], []string{"first", "second"}) {
		t.Fatalf("posts %q", got)
	}
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'post_refused'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d post_refused events", n)
	}
}
