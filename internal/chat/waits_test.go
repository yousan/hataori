package chat

import (
	"fmt"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// Input of a conversation that waits for others says so in each chat thread
// that shows it, with the threads that run: each with a link where it is shown
// in that one's channel, and by its number alone where it is shown in another
// one or nowhere (#875).
func TestInputThatWaitsSaysForWhich(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.ShowIn = map[string][]string{"c9": {channel}}
	})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindConversation)
	conv := f.conv(id)
	here, there := f.bareThread(t, "here"), f.bareThread(t, "there")
	db := f.fe.Outbox.Store.DB
	if _, err := db.Exec(`UPDATE links SET channel = 'c9' WHERE thread_id = ?`, int64(there)); err != nil {
		t.Fatal(err)
	}
	if link, err := f.fe.linkTo(t.Context(), there, conv.Channel); err != nil || link == "" {
		t.Fatalf("it has no link anywhere: %q, %v", link, err)
	}
	other := surface.Conversation{Channel: "c9", Thread: "also"}
	if _, err := db.Exec(`INSERT INTO links (client, channel, thread, thread_id, created_at)
		SELECT client, ?, ?, thread_id, created_at FROM links WHERE thread_id = ?`, other.Channel, other.Thread, int64(id)); err != nil {
		t.Fatal(err)
	}
	f.showing(t)
	f.event(t, id, "input_waits", map[string]any{"running": []control.ThreadID{here, there, 99}})
	f.event(t, id, "text", map[string]any{"text": "after", "final": true})
	line := "This waits until another conversation with the supervisor ends its turn. Running: "
	want := line + fmt.Sprintf("thread %d ([link to %s/here]), thread %d, thread 99.", here, conv.Channel, there)
	if posts := f.postedUntil(t, id, "after"); texts(posts[1:]) != want+"|after" {
		t.Errorf("posted %q, want %q once, after the prompt", texts(posts), want)
	}
	var also []string
	waitFor(t, "the text in the other channel", func() bool {
		also = nil
		for _, c := range f.driver.Calls(other) {
			if c.Op == fake.OpPost {
				also = append(also, c.Message.Text)
			}
		}
		return len(also) > 0 && also[len(also)-1] == "after"
	})
	want = line + fmt.Sprintf("thread %d, thread %d ([link to c9/there]), thread 99.", here, there)
	if len(also) != 2 || also[0] != want {
		t.Errorf("the other channel got %q, want %q once", also, want)
	}
}
