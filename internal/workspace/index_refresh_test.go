package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func staleIndex(t *testing.T) *repoFixture {
	t.Helper()
	f := newRepo(t)
	// Force the default behavior even if a future Git changes it.
	f.git(f.clone, "config", "diff.autoRefreshIndex", "true")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(f.clone, "a"), old, old); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestStateLeavesStaleIndexBytesUnchanged(t *testing.T) {
	f := staleIndex(t)
	index := filepath.Join(f.clone, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if st := f.mustState(); st.Uncommitted != 0 || st.Added != 0 || st.Removed != 0 {
		t.Fatalf("timestamp-only change: %+v", st)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("State rewrote the index after a timestamp-only change")
	}
}

func TestStateDoesNotRunPostIndexChange(t *testing.T) {
	f := staleIndex(t)
	hook := filepath.Join(f.clone, ".git", "hooks", "post-index-change")
	// Keep the marker next to the hook without embedding a shell path.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \"${0%/*}/ran\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.mustState()
	if _, err := os.Lstat(filepath.Join(filepath.Dir(hook), "ran")); !os.IsNotExist(err) {
		t.Fatalf("State ran post-index-change, or marker unreadable: %v", err)
	}
}
