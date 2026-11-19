package chat

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/control"
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
	st := openStore(t, t.TempDir())
	a, b := surface.Conversation{Channel: "c1", Thread: "a"}, surface.Conversation{Channel: "c2", Thread: "b"}
	if _, err := st.DB.Exec(`INSERT INTO threads (id, scope, key, title, repo, started_by, state, created_at, updated_at)
		VALUES (7, 'c1', 'a', '', '', 'x', 'idle', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []surface.Conversation{a, b} {
		if _, err := st.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, made_by, created_at) VALUES (7, 'f', ?, ?, 'x', 0)`,
			c.Channel, c.Thread); err != nil {
			t.Fatal(err)
		}
	}
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
