package chat

import (
	"fmt"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// Input of a conversation that waits for others says so in its chat thread,
// with the threads that run: each with a link where it is shown in the
// channel, and by its number alone where it is shown in another one or
// nowhere (#875).
func TestInputThatWaitsSaysForWhich(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.ShowIn = map[string][]string{"c9": {channel}}
	})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindConversation)
	conv := f.conv(id)
	here, there := f.bareThread(t, "here"), f.bareThread(t, "there")
	if _, err := f.fe.Outbox.Store.DB.Exec(`UPDATE links SET channel = 'c9' WHERE thread_id = ?`, int64(there)); err != nil {
		t.Fatal(err)
	}
	if link, err := f.fe.linkTo(t.Context(), there, conv.Channel); err != nil || link == "" {
		t.Fatalf("it has no link anywhere: %q, %v", link, err)
	}
	f.showing(t)
	f.event(t, id, "input_waits", map[string]any{"running": []control.ThreadID{here, there, 99}})
	f.postedUntil(t, id, fmt.Sprintf("This waits until another conversation with the supervisor ends its turn. Running: "+
		"thread %d ([link to %s/here]), thread %d, thread 99.", here, conv.Channel, there))
}
