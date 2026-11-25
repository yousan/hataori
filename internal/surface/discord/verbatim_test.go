package discord

import (
	"strings"
	"testing"
)

// What Verbatim makes is one block of code, whatever the text holds: the
// text is its characters but for zero-width spaces, and nothing in it ends
// the block, also when a post is split inside it.
func TestVerbatim(t *testing.T) {
	for _, s := range []string{
		"Bash(ls:*), Bash(cat:*)", "Bash([text](https://example.com))", "# a\n> b\n- c", "<@1> <#2> @everyone",
		"`", "``", "```", "````", "`````", "``````", "a```", "```js\nx\n```\n# out", "js\nx", "\\```", " ``` ",
	} {
		v := (&Driver{}).Verbatim(s)
		in, ok := strings.CutPrefix(v, "```\n")
		in, ok2 := strings.CutSuffix(in, "\n```")
		if !ok || !ok2 || strings.Contains(in, "```") {
			t.Errorf("Verbatim(%q) = %q, want one block of code", s, v)
		}
		if got := strings.ReplaceAll(in, "\u200b", ""); got != s {
			t.Errorf("Verbatim(%q) shows %q", s, got)
		}
		if !strings.Contains(s, "```") && in != s {
			t.Errorf("Verbatim(%q) = %q, changed with nothing to break", s, v)
		}
		// Cut inside the block, each part is a block of code of its own.
		long := (&Driver{}).Verbatim(strings.Repeat(s+" ", 400))
		for i, p := range split(long, 200) {
			if !strings.HasPrefix(p, "```\n") || !strings.HasSuffix(p, "```") || strings.Count(p, "```") != 2 {
				t.Fatalf("part %d of %q: %q", i, s, p)
			}
		}
	}
}
