package supervisor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryFingerprintsAndCaps(t *testing.T) {
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("SOUL.md", strings.Repeat("a", UserFileMax+1))
	write("notes/plan.md", "one")
	write("notes/large.md", strings.Repeat("a", FileMax+1))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "notes", "link")); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]string {
		t.Helper()
		got, err := s.memory(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := read()
	if !strings.HasPrefix(before["SOUL.md"], "stat:") || !strings.HasPrefix(before["notes/large.md"], "stat:") || strings.HasPrefix(before["notes/plan.md"], "stat:") {
		t.Fatalf("fingerprints: %v", before)
	}
	// Same-length content changes are found even if mtime is unchanged.
	st, err := os.Stat(filepath.Join(dir, "notes/plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	write("notes/plan.md", "two")
	if err := os.Chtimes(filepath.Join(dir, "notes/plan.md"), st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	after := read()
	if after["notes/plan.md"] == before["notes/plan.md"] {
		t.Fatal("a content change with unchanged metadata was lost")
	}
	// Skipped large files change on mtime or size, without reading them.
	later := st.ModTime().Add(time.Hour)
	for _, name := range []string{"SOUL.md", "notes/large.md"} {
		if err := os.Chtimes(filepath.Join(dir, name), later, later); err != nil {
			t.Fatal(err)
		}
	}
	after = read()
	for _, name := range []string{"SOUL.md", "notes/large.md"} {
		if before[name] == after[name] {
			t.Fatalf("metadata change lost: %s", name)
		}
	}
	if err := os.Remove(filepath.Join(dir, "notes/plan.md")); err != nil {
		t.Fatal(err)
	}
	changes := memoryChanges(1, before, read())
	found := false
	for _, c := range changes {
		if c.Name == "notes/plan.md" && c.Now == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("removed note not named")
	}
	for name := range after {
		if strings.Contains(name, "link") {
			t.Fatalf("followed link: %q", name)
		}
	}
	many := map[string]string{}
	for i := range memoryNoticeMax + 5 {
		many[strings.Repeat("a", i+1)] = "hash"
	}
	told := map[string]string{}
	first := memoryChanges(1, told, many)
	if len(first) != memoryNoticeMax {
		t.Fatalf("cap: %d", len(first))
	}
	for _, c := range first {
		told[c.Name] = c.Now
	}
	if len(memoryChanges(1, told, many)) != 5 {
		t.Fatal("capped changes were lost")
	}
}
