package holder

import (
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// The bytes hataori's own host writes for each part are the bytes the
// Claude Code codec wrote itself before it returned parts: a change here
// changes what reaches every agent in a terminal.
func TestHostEncoding(t *testing.T) {
	for _, c := range []struct {
		part agent.Part
		want string
	}{
		{agent.Part{Paste: "one line"}, "\x1b[200~one line\x1b[201~"},
		{agent.Part{Paste: "two\nlines\twith a tab"}, "\x1b[200~two\nlines\twith a tab\x1b[201~"},
		{agent.Part{Paste: "長い入力"}, "\x1b[200~長い入力\x1b[201~"},
		{agent.Part{}, "\x1b[200~\x1b[201~"},
		{agent.Part{Key: agent.Enter}, "\r"},
		{agent.Part{Key: agent.Escape}, "\x1b"},
		{agent.Part{Key: agent.Up}, "\x1b[A"},
		{agent.Part{Key: agent.Down}, "\x1b[B"},
	} {
		got, err := Encode(c.part)
		if err != nil || string(got) != c.want {
			t.Errorf("%+v: %q, %v; want %q", c.part, got, err, c.want)
		}
	}
	if len(keys) != 4 {
		t.Errorf("%d keys in the table; each needs a line above", len(keys))
	}
}

// A key the table does not have refuses the whole input: nothing is
// written, not even the parts before it.
func TestHostRefusesAnUnknownKey(t *testing.T) {
	for _, p := range []agent.Part{{Key: "Tab"}, {Key: agent.Enter, Paste: "both"}} {
		if _, err := Encode(p); err == nil {
			t.Errorf("%+v encoded", p)
		}
	}
	// The window names no host: had Input tried to write, the error would be
	// about reaching it.
	err := (&Host{}).Input(t.Context(), Window{Holder: HostName, Ref: t.TempDir(), Owned: true},
		[]agent.Part{{Paste: "first"}, {Key: "Tab"}})
	if err == nil || !strings.Contains(err.Error(), `no key "Tab"`) {
		t.Fatalf("input with an unknown key: %v", err)
	}
}
