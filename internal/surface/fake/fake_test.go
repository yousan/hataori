package fake

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
)

type sink struct {
	mu  sync.Mutex
	got []surface.Inbound
}

func (s *sink) Message(in surface.Inbound) { s.mu.Lock(); s.got = append(s.got, in); s.mu.Unlock() }
func (s *sink) Choice(surface.Selection)   {}
func (s *sink) Command(surface.Command)    {}
func (s *sink) all() []surface.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]surface.Inbound(nil), s.got...)
}

// connect runs d until the test ends and waits until it is connected.
func connect(t *testing.T, d *Driver) *sink {
	t.Helper()
	s := &sink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, s); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); ; {
		d.mu.Lock()
		ok := d.connected
		d.mu.Unlock()
		if ok {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not connect")
		}
		time.Sleep(time.Millisecond)
	}
}

var (
	ch  = surface.Conversation{Channel: "c1"}
	ctx = context.Background()
)

func TestRecordsCallsInOrder(t *testing.T) {
	d := New()
	th, err := d.OpenThread(ctx, ch, "title")
	if err != nil || th.Channel != "c1" || th.Thread == "" {
		t.Fatalf("OpenThread = %+v, %v", th, err)
	}
	r1, _ := d.Post(ctx, th, "k1", surface.Message{Text: "one"})
	r2, _ := d.Post(ctx, th, "k2", surface.Message{Text: "two"})
	if r1 == r2 || r1.Conv != th {
		t.Fatalf("refs %+v %+v", r1, r2)
	}
	if err := d.Edit(ctx, r1, surface.Message{Text: "one'"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStatus(ctx, r2, surface.Done); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if err := d.SetThread(ctx, th, surface.ThreadMeta{Title: "t", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.Edit(ctx, r2, surface.Message{}); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("edit after delete: %v", err)
	}

	want := []Call{
		{Op: OpPost, Ref: r1, Key: "k1", Message: surface.Message{Text: "one"}},
		{Op: OpPost, Ref: r2, Key: "k2", Message: surface.Message{Text: "two"}},
		{Op: OpEdit, Ref: r1, Message: surface.Message{Text: "one'"}},
		{Op: OpSetStatus, Ref: r2, Status: surface.Done},
		{Op: OpDelete, Ref: r2},
		{Op: OpSetThread, Thread: surface.ThreadMeta{Title: "t", Archived: true}},
	}
	if got := d.Calls(th); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls in thread:\n got %+v\nwant %+v", got, want)
	}
	if got := d.Calls(ch); len(got) != 1 || got[0].Op != OpOpenThread || got[0].Opened != th || got[0].Title != "title" {
		t.Fatalf("calls in channel: %+v", got)
	}
}

func TestInjectAuthorsAndPlaces(t *testing.T) {
	d := New()
	s := connect(t, d)
	th := surface.Conversation{Channel: "c1", Thread: "t1"}
	d.Inject(ch, Webhook("w"), "from a webhook")
	d.Inject(th, Person("p"), "from a person")
	d.Inject(th, Bot("b"), "from a bot")

	got := s.all()
	if len(got) != 3 {
		t.Fatalf("delivered %d", len(got))
	}
	for i, w := range []struct {
		conv surface.Conversation
		kind surface.AuthorKind
	}{{ch, surface.Webhook}, {th, surface.Person}, {th, surface.Bot}} {
		if got[i].Ref.Conv != w.conv || got[i].Author.Kind != w.kind {
			t.Errorf("message %d: %+v", i, got[i])
		}
	}
}

func TestSinceAfterOutage(t *testing.T) {
	d := New()
	s := connect(t, d)
	first := d.Inject(ch, Person("p"), "live")
	d.Disconnect()
	d.Inject(ch, Person("p"), "missed 1")
	d.Inject(ch, Person("p"), "missed 2")
	d.Reconnect()

	if n := len(s.all()); n != 1 {
		t.Fatalf("delivered %d, want only the live one", n)
	}
	got, err := d.Since(ctx, ch, first.Cursor)
	if err != nil || len(got) != 2 || got[0].Text != "missed 1" || got[1].Text != "missed 2" {
		t.Fatalf("Since = %+v, %v", got, err)
	}
}

func TestFailures(t *testing.T) {
	d := New()
	d.Fail(OpEdit, 2, nil)
	ref, _ := d.Post(ctx, ch, "", surface.Message{Text: "x"})
	for i := 0; i < 2; i++ {
		if err := d.Edit(ctx, ref, surface.Message{}); !errors.Is(err, ErrInjected) {
			t.Fatalf("edit %d: %v", i, err)
		}
	}
	if err := d.Edit(ctx, ref, surface.Message{}); err != nil {
		t.Fatalf("third edit: %v", err)
	}

	lost := errors.New("answer lost")
	d.FailAfter(OpPost, 1, lost)
	if _, err := d.Post(ctx, ch, "k", surface.Message{Text: "y"}); !errors.Is(err, lost) {
		t.Fatalf("post: %v", err)
	}
	calls := d.Calls(ch)
	if last := calls[len(calls)-1]; last.Op != OpPost || last.Message.Text != "y" {
		t.Fatalf("the failed post is not on the surface: %+v", calls)
	}
	if len(calls) != 3 { // post x, edit, post y: the failed edits changed nothing
		t.Fatalf("calls: %+v", calls)
	}
}

func TestLimits(t *testing.T) {
	d := New()
	d.SetLimits(surface.Limits{MaxText: 10})
	if d.Limits().MaxText != 10 {
		t.Fatal("limit not set")
	}
}

func TestNotInTheCommand(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/yousan/hataori/cmd/hataori").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	if bytes.Contains(out, []byte("internal/surface/fake")) {
		t.Fatal("cmd/hataori depends on the fake surface")
	}
}
