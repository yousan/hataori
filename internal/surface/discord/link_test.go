package discord

import (
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

// A link is the mention of the thread, or of the channel when there is no
// thread; a conversation whose id is none of Discord's has no link.
func TestLink(t *testing.T) {
	for conv, want := range map[surface.Conversation]string{
		{Channel: "100", Thread: "200"}: "<#200>",
		{Channel: "100"}:                "<#100>",
		{Channel: "c", Thread: "t"}:     "",
	} {
		if got := (&Driver{}).Link(conv); got != want {
			t.Errorf("Link(%v) = %q, want %q", conv, got, want)
		}
	}
}
