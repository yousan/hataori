package chat

import (
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// Input of a conversation that waits for others says so in its chat thread,
// with the threads that run, each with a link to it where it is shown in
// chat (#875).
func TestInputThatWaitsSaysForWhich(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindConversation)
	conv := f.conv(id)
	f.showing(t)
	f.event(t, id, "input_waits", map[string]any{"running": []control.ThreadID{id, 99}})
	f.postedUntil(t, id, "This waits until another conversation with the supervisor ends its turn. Running: thread 1 ([link to "+
		conv.Channel+"/"+conv.Thread+"]), thread 99.")
}
