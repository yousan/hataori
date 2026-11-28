package chat

import (
	"context"
	"testing"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// cancelAfterPost returns a successful service reply as its sender stops.
type cancelAfterPost struct {
	*fake.Driver
	cancel context.CancelFunc
}

func (d *cancelAfterPost) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	ref, err := d.Driver.Post(ctx, conv, key, m)
	if err == nil {
		d.cancel()
	}
	return ref, err
}

// Shutdown records the post whose successful reply it received, leaves the
// rest queued, and a fresh outbox posts only that remaining row.
func TestShutdownRecordsOnlyTheCompletedPost(t *testing.T) {
	st := openStore(t, t.TempDir())
	defer st.Close()
	sending, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelAfterPost{Driver: fake.New(), cancel: cancel}
	o := &Outbox{Store: st, Client: "x", Driver: d}
	if err := o.Write(ctx, row(1, 0, ta, "first"), row(2, 0, ta, "second")); err != nil {
		t.Fatal(err)
	}
	o.drain(sending, ta)
	var first, second string
	if err := st.DB.QueryRow(`SELECT state FROM chat_outbox WHERE event = 1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(`SELECT state FROM chat_outbox WHERE event = 2`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != "sent" || second != "pending" {
		t.Errorf("after shutdown: first %s, second %s; want sent, pending", first, second)
	}
	if posts := postsIn(d.Driver, ta); len(posts) != 1 || posts[0] != "first" {
		t.Errorf("before restart posted %v, want only first", posts)
	}
	again := &Outbox{Store: st, Client: "x", Driver: d.Driver}
	again.init()
	again.drain(ctx, ta)
	if posts := postsIn(d.Driver, ta); len(posts) != 2 || posts[0] != "first" || posts[1] != "second" {
		t.Errorf("after restart posted %v, want first, second", posts)
	}
}
