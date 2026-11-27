package claudecode

import (
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// These are inputs, not invented CLI output.
func TestImagePathLinesAreText(t *testing.T) {
	for _, confine := range []*agent.Confinement{nil, {}} {
		c := (&Adapter{}).NewCodec(agent.Spec{Confine: confine}).(*codec)
		for _, tt := range []struct{ text, want string }{
			{"/tmp/example.png", commandPrefix + "/tmp/example.png"},
			{"Look at this\n/tmp/example.png", "Look at this\n" + commandPrefix + "/tmp/example.png"},
			{"Look at this\n  '/tmp/with spaces.JPEG'  \nthen answer", "Look at this\n" + commandPrefix + "  '/tmp/with spaces.JPEG'  \nthen answer"},
			{"Look at these\n/tmp/one.gif /tmp/two.webp", "Look at these\n" + commandPrefix + "/tmp/one.gif /tmp/two.webp"},
			{"Look at these\n/one.png /tmp/two.webp", "Look at these\n" + commandPrefix + "/one.png /tmp/two.webp"},
			{"Look at this\n/tmp/escaped.p\\ng", "Look at this\n" + commandPrefix + "/tmp/escaped.p\\ng"},
		} {
			got := c.pasteText(tt.text)
			if got != tt.want {
				t.Errorf("confined=%v: %q pasted as %q, want %q", confine != nil, tt.text, got, tt.want)
			}
			if twice := noImagePaths(got); twice != got {
				t.Errorf("confined=%v: pasting twice changes %q into %q", confine != nil, got, twice)
			}
		}
		for _, text := range []string{"Say ok", "Look at /tmp/example.png please", "Look at /tmp/example.png", "/help", "/review /tmp/example.png", "Text\n> /tmp/example.png",
			"Look at this\n\u200b/tmp/example.png", "Look at this\n/tmp/example.p\u200bng", "Look at this\n/tmp/example.png\u200b"} {
			want := text
			if confine != nil {
				want = noCommand(text)
			}
			if got := c.pasteText(text); got != want {
				t.Errorf("confined=%v: %q pasted as %q, want %q", confine != nil, text, got, want)
			}
		}
	}
}

func TestOrdinaryShellKeepsImagePaths(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{}).(*codec)
	for _, text := range []string{"!cat /tmp/example.png", "!cat example.png", "!printf ok\nmore text"} {
		if got := c.pasteText(text); got != text {
			t.Errorf("shell command %q pasted as %q", text, got)
		}
	}
}

func TestOrdinaryShellDoesNotHideImageLines(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{}).(*codec)
	for _, tt := range []struct{ text, want string }{
		{"!cat\n/tmp/example.png", "!cat\n" + commandPrefix + "/tmp/example.png"},
		{"!cat example.png\n/tmp/example.png", "!cat example.png\n" + commandPrefix + "/tmp/example.png"},
		{"\n  !cat example.png\n/tmp/example.png", "\n  !cat example.png\n" + commandPrefix + "/tmp/example.png"},
	} {
		if got := c.pasteText(tt.text); got != tt.want {
			t.Errorf("%q pasted as %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestOrdinaryImagePathAfterTerminalSequence(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{}).(*codec)
	text := "Look at this\n\x1b[31m/tmp/example.png"
	want := "Look at this\n" + commandPrefix + "\x1b[31m/tmp/example.png"
	if got := c.pasteText(text); got != want {
		t.Fatalf("%q pasted as %q, want %q", text, got, want)
	}
}
