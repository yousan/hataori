package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface/fake"
)

// dirs makes a workspace, a state directory and a directory outside both,
// and gives the front end the first two as its file roots.
func withDirs(t *testing.T) (ws, state, outside string, opt func(*FrontEnd)) {
	base := t.TempDir()
	ws, state, outside = filepath.Join(base, "ws"), filepath.Join(base, "state"), filepath.Join(base, "outside")
	for _, d := range []string{ws, state, outside} {
		os.MkdirAll(d, 0o700)
	}
	return ws, state, outside, func(fe *FrontEnd) {
		fe.FileRoots = func(context.Context, control.ThreadID, int64) ([]string, error) { return []string{ws, state}, nil }
	}
}

func write(t *testing.T, path string, n int) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("x", n)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileCalls(f *front, id control.ThreadID) []fake.Call {
	var out []fake.Call
	for _, c := range f.driver.Calls(conv(id)) {
		if c.Op == fake.OpPost && c.Message.Text != "the prompt" {
			out = append(out, c)
		}
	}
	return out
}

func names(c fake.Call) string {
	var s []string
	for _, f := range c.Message.Files {
		s = append(s, f.Name)
	}
	return strings.Join(s, ",")
}

func TestFilesArePostedWithTheirCaptionSpreadOverMessages(t *testing.T) {
	ws, state, _, opt := withDirs(t)
	f := newFront(t, workspaces{}, opt)
	id, _ := f.started(t)
	f.showing(t)
	var paths []any
	for i := 1; i <= 12; i++ {
		dir := ws
		if i == 12 {
			dir = state
		}
		paths = append(paths, write(t, filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), 10))
	}
	f.event(t, id, "file_out", map[string]any{"file": paths[0], "more": paths[1:], "caption": "The results."})
	f.event(t, id, "text", map[string]any{"text": "marker", "final": false})
	f.postedUntil(t, id, "marker")
	calls := fileCalls(f, id)
	if len(calls) != 3 { // 10 files (the fake's limit), 2 files, the marker
		t.Fatalf("calls %+v", calls)
	}
	var got []string
	for _, c := range calls[:len(calls)-1] {
		got = append(got, c.Message.Text+"["+names(c)+"]")
	}
	want := "The results.[f01.txt,f02.txt,f03.txt,f04.txt,f05.txt,f06.txt,f07.txt,f08.txt,f09.txt,f10.txt]|[f11.txt,f12.txt]"
	if strings.Join(got, "|") != want {
		t.Fatalf("messages\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
}

func TestFileOverTheLimitIsNamedNotPosted(t *testing.T) {
	ws, _, _, opt := withDirs(t)
	f := newFront(t, workspaces{}, opt)
	l := fake.DefaultLimits
	l.MaxFileBytes = 1000
	f.driver.SetLimits(l)
	id, _ := f.started(t)
	f.showing(t)
	small, big := write(t, filepath.Join(ws, "small.txt"), 10), write(t, filepath.Join(ws, "big.bin"), 5000)
	f.event(t, id, "file_out", map[string]any{"file": big, "more": []string{small}})
	f.event(t, id, "text", map[string]any{"text": "marker", "final": false})
	f.postedUntil(t, id, "marker")
	calls := fileCalls(f, id)
	if len(calls) != 3 || names(calls[0]) != "small.txt" ||
		calls[1].Message.Text != "File big.bin was not posted: 5 KiB is over the limit of 1 KiB." {
		t.Fatalf("calls %+v", calls)
	}
}

func TestFilesOutsideTheRootsAreRefusedAlsoThroughASymlink(t *testing.T) {
	ws, _, outside, opt := withDirs(t)
	f := newFront(t, workspaces{}, opt)
	id, _ := f.started(t)
	f.showing(t)
	secret := write(t, filepath.Join(outside, "secret.txt"), 10)
	link := filepath.Join(ws, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(ws, "out")
	os.Symlink(outside, dirLink)
	f.event(t, id, "file_out", map[string]any{"file": secret, "more": []string{link, filepath.Join(dirLink, "secret.txt"), ws + "/../outside/secret.txt"}})
	f.event(t, id, "text", map[string]any{"text": "marker", "final": false})
	f.postedUntil(t, id, "marker")
	calls := fileCalls(f, id)
	if len(calls) != 5 {
		t.Fatalf("calls %+v", calls)
	}
	for _, c := range calls[:4] {
		if len(c.Message.Files) != 0 || !strings.Contains(c.Message.Text, "outside the workspace") {
			t.Fatalf("call %+v, want a refusal", c)
		}
	}
}
