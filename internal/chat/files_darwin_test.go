package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// A root spelled through a firmlink, as /System/Volumes/Data/private/var
// for /private/var, still has its files posted.
func TestFileUnderAFirmlinkIsPosted(t *testing.T) {
	ws, _, _, opt := withDirs(t)
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join("/System/Volumes/Data", real)
	if _, err := os.Stat(data); err != nil {
		t.Skip("the workspace is not behind a firmlink here:", err)
	}
	f := newFront(t, workspaces{}, opt, func(fe *FrontEnd) {
		fe.FileRoots = func(context.Context, control.ThreadID, int64) ([]string, error) { return []string{data}, nil }
	})
	id, _ := f.started(t)
	f.showing(t)
	report := write(t, filepath.Join(data, "report.txt"), 3)
	f.event(t, id, "file_out", map[string]any{"file": report})
	f.event(t, id, "text", map[string]any{"text": "marker", "final": false})
	f.postedUntil(t, id, "marker")
	if c := fileCalls(f, id); len(c) != 2 || names(c[0]) != "report.txt" {
		t.Fatalf("calls %+v", c)
	}
}
