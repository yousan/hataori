package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// A session's state directory is never removed when it and the workspace
// hold one another, links resolved.
func TestNested(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state", "sessions", "4")
	os.MkdirAll(state, 0o700)
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(root, link)
	for _, c := range []struct {
		workspace string
		want      bool
	}{
		{root, true},                          // the workspace holds the state directory
		{link, true},                          // the same, through a link
		{state, true},                         // the same directory
		{filepath.Join(state, "sub"), true},   // in it, even when it is gone...
		{other, false},                        // elsewhere
		{filepath.Join(other, "gone"), false}, // elsewhere and gone
	} {
		if c.workspace == filepath.Join(state, "sub") {
			os.MkdirAll(c.workspace, 0o700) // ...it is there here
		}
		if got := nested(state, c.workspace); got != c.want {
			t.Errorf("nested(state, %s) = %v, want %v", c.workspace, got, c.want)
		}
	}
}
