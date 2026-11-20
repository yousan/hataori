package chat

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

func openStore(t *testing.T, root string) *store.Store {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// flaky is the fake driver with posts that fail while fail says so.
type flaky struct {
	*fake.Driver
	mu       sync.Mutex
	fail     func(conv surface.Conversation, m surface.Message) bool
	attempts map[surface.Conversation][]string // keys of every Post, failed or not
}

func newFlaky() *flaky {
	return &flaky{Driver: fake.New(), attempts: map[surface.Conversation][]string{}}
}

func (f *flaky) setFail(fn func(surface.Conversation, surface.Message) bool) {
	f.mu.Lock()
	f.fail = fn
	f.mu.Unlock()
}

func (f *flaky) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	f.mu.Lock()
	f.attempts[conv] = append(f.attempts[conv], key)
	bad := f.fail != nil && f.fail(conv, m)
	f.mu.Unlock()
	if bad {
		return surface.MessageRef{}, fake.ErrInjected
	}
	return f.Driver.Post(ctx, conv, key, m)
}

func (f *flaky) tries(conv surface.Conversation) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attempts[conv]...)
}

// run runs o until the returned function is called.
func run(t *testing.T, o *Outbox) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func postsIn(d *fake.Driver, conv surface.Conversation) []string {
	var out []string
	for _, c := range d.Calls(conv) {
		if c.Op == fake.OpPost {
			out = append(out, c.Message.Text)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitWhileBusy waits for ok as long as what busy returns keeps changing:
// it fails after five seconds in which ok stayed false and busy stayed the
// same. A slow machine takes longer to do the same work, which busy shows;
// a front end that is stuck shows nothing new. One that keeps changing
// without ever getting there fails after two minutes.
func waitWhileBusy(t *testing.T, what string, busy func() string, ok func() bool) {
	t.Helper()
	last := busy()
	limit := time.Now().Add(2 * time.Minute)
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if now := busy(); now != last {
			last, deadline = now, time.Now().Add(5*time.Second)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s, nothing done for 5s: %s", what, last)
		}
		if time.Now().After(limit) {
			t.Fatalf("timed out waiting for %s, still busy after 2 minutes: %s", what, last)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var (
	ctx = context.Background()
	ta  = surface.Conversation{Channel: "c", Thread: "a"}
	tb  = surface.Conversation{Channel: "c", Thread: "b"}
)

func row(event uint64, part int, conv surface.Conversation, text string) Row {
	return Row{Event: event, Part: part, Conv: conv, Message: surface.Message{Text: text}}
}

func countRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT count(*) FROM chat_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWriteSameKeyKeepsOneRow(t *testing.T) {
	st := openStore(t, t.TempDir())
	defer st.Close()
	o := &Outbox{Store: st, Client: "x", Driver: fake.New()}
	if err := o.Write(ctx, row(1, 0, ta, "first")); err != nil {
		t.Fatal(err)
	}
	if err := o.Write(ctx, row(1, 0, ta, "second"), row(1, 1, ta, "other part")); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
	var text string
	st.DB.QueryRow(`SELECT message FROM chat_outbox WHERE event = 1 AND part = 0`).Scan(&text)
	if !strings.Contains(text, `"first"`) {
		t.Fatalf("row (1, 0) holds %s, want the first write", text)
	}
}

func TestOrderPerThreadAndAFailingThreadHoldsBackOnlyItself(t *testing.T) {
	st := openStore(t, t.TempDir())
	defer st.Close()
	d := newFlaky()
	d.setFail(func(c surface.Conversation, _ surface.Message) bool { return c == ta })
	o := &Outbox{Store: st, Client: "x", Driver: d, Retry: 5 * time.Millisecond}
	if err := o.Write(ctx, row(1, 0, ta, "a1"), row(2, 0, tb, "b1"), row(3, 0, ta, "a2"), row(4, 0, tb, "b2"), row(5, 0, tb, "b3")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "thread b's posts", func() bool { return len(postsIn(d.Driver, tb)) == 3 })
	if got := strings.Join(postsIn(d.Driver, tb), ","); got != "b1,b2,b3" {
		t.Fatalf("thread b got %s", got)
	}
	waitFor(t, "a retry in thread a", func() bool { return len(d.tries(ta)) >= 3 })
	for _, k := range d.tries(ta) {
		if k != "1.0" {
			t.Fatalf("thread a tried %v: a2 was posted while a1 failed", d.tries(ta))
		}
	}
	if p := postsIn(d.Driver, ta); len(p) != 0 {
		t.Fatalf("thread a got %v while its posts fail", p)
	}
	d.setFail(nil)
	waitFor(t, "thread a's posts", func() bool { return len(postsIn(d.Driver, ta)) == 2 })
	if got := strings.Join(postsIn(d.Driver, ta), ","); got != "a1,a2" {
		t.Fatalf("thread a got %s", got)
	}
}

func TestSentRefIsStoredAndNotPostedAfterRestart(t *testing.T) {
	root := t.TempDir()
	st := openStore(t, root)
	d := fake.New()
	o := &Outbox{Store: st, Client: "x", Driver: d, Retry: 5 * time.Millisecond}
	if err := o.Write(ctx, row(1, 0, ta, "one"), row(2, 0, ta, "two")); err != nil {
		t.Fatal(err)
	}
	stop := run(t, o)
	waitFor(t, "two posts", func() bool { return len(postsIn(d, ta)) == 2 })
	waitFor(t, "both refs", func() bool {
		_, ok, _ := o.Sent(ctx, 2, 0)
		return ok
	})
	calls := d.Calls(ta)
	for i, c := range calls {
		r, ok, err := o.Sent(ctx, uint64(i+1), 0)
		if err != nil || !ok || r != c.Ref {
			t.Fatalf("Sent(%d, 0) = %+v, %v, %v; the post was %+v", i+1, r, ok, err, c.Ref)
		}
	}
	stop()
	st.Close()

	st = openStore(t, root)
	defer st.Close()
	o = &Outbox{Store: st, Client: "x", Driver: d, Retry: 5 * time.Millisecond}
	run(t, o)
	// The sender posts a thread's rows in order, so once a new row is
	// posted, any old row it would post again is posted too.
	if err := o.Write(ctx, row(3, 0, ta, "three")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new row", func() bool { _, ok, _ := o.Sent(ctx, 3, 0); return ok })
	if got := strings.Join(postsIn(d, ta), ","); got != "one,two,three" {
		t.Fatalf("posts after the restart: %s", got)
	}
}

func TestDecorationIsDroppedAndNeverHoldsBackAReply(t *testing.T) {
	st := openStore(t, t.TempDir())
	defer st.Close()
	d := newFlaky()
	d.setFail(func(_ surface.Conversation, m surface.Message) bool { return m.Text == "lamp" })
	o := &Outbox{Store: st, Client: "x", Driver: d, Retry: 5 * time.Millisecond, MaxFailures: 3}
	deco := row(1, 0, ta, "lamp")
	deco.Decoration = true
	if err := o.Write(ctx, deco, row(2, 0, ta, "reply")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "the reply", func() bool { return len(postsIn(d.Driver, ta)) == 1 })
	waitFor(t, "the decoration to be dropped", func() bool {
		var state string
		st.DB.QueryRow(`SELECT state FROM chat_outbox WHERE event = 1`).Scan(&state)
		return state == "dropped"
	})
	// One more tick: a dropped row is not tried again.
	time.Sleep(30 * time.Millisecond)
	n := 0
	for _, k := range d.tries(ta) {
		if k == "1.0" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("the decoration was tried %d times, want 3", n)
	}
}

func TestPostReportedFailedIsRetriedWithTheSameKey(t *testing.T) {
	st := openStore(t, t.TempDir())
	defer st.Close()
	d := fake.New()
	d.FailAfter(fake.OpPost, 1, nil) // the post happens, the driver reports a failure
	o := &Outbox{Store: st, Client: "x", Driver: d, Retry: 5 * time.Millisecond}
	if err := o.Write(ctx, row(7, 2, ta, "reply")); err != nil {
		t.Fatal(err)
	}
	run(t, o)
	waitFor(t, "the row to be sent", func() bool { _, ok, _ := o.Sent(ctx, 7, 2); return ok })
	calls := d.Calls(ta)
	if len(calls) != 2 || calls[0].Key != "7.2" || calls[1].Key != "7.2" {
		t.Fatalf("calls %+v, want two posts with key 7.2", calls)
	}
	if r, _, _ := o.Sent(ctx, 7, 2); r != calls[1].Ref {
		t.Fatalf("stored %+v, want the ref of the post that succeeded, %+v", r, calls[1].Ref)
	}
}

// The package is shared by every surface and agent, so it names none.
func TestNoSurfaceOrAgentNames(t *testing.T) {
	// Spelled in pieces so that this file passes its own check.
	names := []string{"dis" + "cord", "sl" + "ack", "tea" + "ms", "cla" + "ude", "co" + "dex"}
	re := regexp.MustCompile(`(?i)` + strings.Join(names, "|"))
	files, _ := filepath.Glob("*.go")
	if len(files) == 0 {
		t.Fatal("no files")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.Find(data); m != nil {
			t.Errorf("%s names %q", f, m)
		}
	}
}
