package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/workspace"
)

// With cleanup.trash_retention set, the daemon deletes the entries of
// trash/ that the removal named and that are older than the retention:
// shortly after start, and again on later passes, one log line for each.
// An entry it did not name, a pending one and one within the retention
// stay. An entry that cannot be deleted is logged and deleted on a later
// pass once it can be.
func TestDaemonPurgesOldTrash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not keep root from deleting in it")
	}
	root := newRoot(t)
	var cfg map[string]any
	b, _ := os.ReadFile(filepath.Join(root, instance.ConfigFile))
	json.Unmarshal(b, &cfg)
	cfg["cleanup"] = map[string]any{"trash_retention": "1h"}
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, instance.ConfigFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(root, workspace.TrashDir)
	entry := func(name string) string {
		p := filepath.Join(trash, name)
		if err := os.MkdirAll(filepath.Join(p, "d"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "d", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	recent := time.Now().UTC().Add(-time.Minute).Format("20060102T150405Z") + "-0123abcd-k"
	old := entry("20000101T000000Z-0123abcd-k")
	keep := []string{entry("not-from-removal"), entry("pending-20000101T000000Z-0123abcd-k"), entry(recent)}
	// One whose content cannot be deleted until the test allows it.
	stuck := entry("20000101T000000Z-89abcdef-stuck")
	if err := os.Chmod(filepath.Join(stuck, "d"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(stuck, "d"), 0o700) })

	purgeEvery = 200 * time.Millisecond
	t.Cleanup(func() { purgeEvery = 0 })
	logged := startDaemonLog(t, root)
	gone := func(p string) func() bool {
		return func() bool { _, err := os.Lstat(p); return os.IsNotExist(err) }
	}
	eventually(t, "the purge at start", 10*time.Second, gone(old))
	// The root may be reached through a link, as on macOS, so the log is
	// matched by the entry's name in trash/.
	names := func(p string) *regexp.Regexp {
		return regexp.MustCompile(regexp.QuoteMeta(string(filepath.Separator)+workspace.TrashDir+string(filepath.Separator)+filepath.Base(p)) + `([:" ]|$)`)
	}
	eventually(t, "the failed purge logged", 10*time.Second, func() bool {
		for _, l := range strings.Split(logged(), "\n") {
			if strings.Contains(l, "level=ERROR") && strings.Contains(l, "trash purge") && names(stuck).MatchString(l) {
				return true
			}
		}
		return false
	})
	if _, err := os.Lstat(stuck); err != nil {
		t.Fatalf("the entry that cannot be deleted: %v", err)
	}

	// A later pass deletes an entry put there after the start, and the one
	// that can be deleted now.
	later := entry("20000102T000000Z-0123abcd-later")
	os.Chmod(filepath.Join(stuck, "d"), 0o700)
	eventually(t, "a later purge", 10*time.Second, func() bool { return gone(later)() && gone(stuck)() })

	for _, p := range keep {
		if _, err := os.Lstat(filepath.Join(p, "d", "f")); err != nil {
			t.Errorf("%s was deleted: %v", filepath.Base(p), err)
		}
	}
	// An entry is deleted before its line is written, so the lines are
	// waited for, and counted after one more pass.
	count := func(p string) int {
		n := 0
		for _, l := range strings.Split(logged(), "\n") {
			if strings.Contains(l, "msg=\"trash purged\"") && names(p).MatchString(l) {
				n++
			}
		}
		return n
	}
	all := []string{old, later, stuck}
	eventually(t, "the log lines", 10*time.Second, func() bool {
		for _, p := range all {
			if count(p) == 0 {
				return false
			}
		}
		return true
	})
	time.Sleep(3 * purgeEvery)
	for _, p := range all {
		if n := count(p); n != 1 {
			t.Errorf("%d log lines name %s as purged, want 1", n, filepath.Base(p))
		}
	}
}
