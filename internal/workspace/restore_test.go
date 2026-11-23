package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// removed is a removal fixture whose workspace, with an unpushed commit
// and an untracked file in it, was moved into trash as Remove does.
func removed(t *testing.T) (*removal, string, map[string]string) {
	t.Helper()
	x := newRemoval(t)
	x.r.Disposable = []string{"*.log"}
	x.write("build.log", "x\n")
	to, err := x.r.Remove(context.Background(), x.clone)
	if err != nil {
		t.Fatal(err)
	}
	// Work that exists nowhere else, as a person's would after a removal
	// that a later commit in trash would not have passed: the restore
	// must bring back whatever is there.
	x.commit(to, "later")
	os.WriteFile(filepath.Join(to, "notes.txt"), []byte("mine\n"), 0o600)
	return x, to, treeSnapshot(t, to)
}

// Restore takes the workspace back to its place, the same directory with
// everything in it, and trash no longer holds it.
func TestRestoreTakesTheWorkspaceBack(t *testing.T) {
	x, to, inside := removed(t)
	st, _ := os.Lstat(to)
	if err := x.r.Restore(context.Background(), to, x.clone, nil); err != nil {
		t.Fatal(err)
	}
	back, err := os.Lstat(x.clone)
	if err != nil || !os.SameFile(st, back) {
		t.Fatalf("the workspace at its place: %v, the same directory %v", err, err == nil && os.SameFile(st, back))
	}
	if got := treeSnapshot(t, x.clone); fmt.Sprint(got) != fmt.Sprint(inside) {
		t.Fatal("the restored workspace's contents differ")
	}
	if _, err := os.Lstat(to); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("trash still holds it: %v", err)
	}
}

// Restore never replaces anything: a directory, an empty one too, or a
// file at the workspace's place is refused, and it and the entry in trash
// stay as they are. An entry the removal did not make, a link, an entry
// outside trash and a place outside the workspaces directory are refused
// too. An entry that is gone, as after the purge, says so.
func TestRestoreRefusesAndOverwritesNothing(t *testing.T) {
	ctx := context.Background()
	x, to, inside := removed(t)
	for name, put := range map[string]func(string) error{
		"an empty directory": func(p string) error { return os.Mkdir(p, 0o700) },
		"a directory": func(p string) error {
			os.Mkdir(p, 0o700)
			return os.WriteFile(filepath.Join(p, "other"), []byte("other\n"), 0o600)
		},
		"a file": func(p string) error { return os.WriteFile(p, []byte("other\n"), 0o600) },
		"a link": func(p string) error { return os.Symlink(to, p) },
	} {
		if err := put(x.clone); err != nil {
			t.Fatal(err)
		}
		there := treeSnapshot(t, filepath.Dir(x.clone))
		err := x.r.Restore(ctx, to, x.clone, nil)
		if !errors.Is(err, ErrExists) {
			t.Errorf("%s at the place: %v, want ErrExists", name, err)
		}
		if got := treeSnapshot(t, filepath.Dir(x.clone)); fmt.Sprint(got) != fmt.Sprint(there) {
			t.Errorf("%s at the place: it changed", name)
		}
		if got := treeSnapshot(t, to); fmt.Sprint(got) != fmt.Sprint(inside) {
			t.Fatalf("%s at the place: the entry in trash changed", name)
		}
		os.RemoveAll(x.clone)
	}

	trash := filepath.Dir(to)
	link := filepath.Join(trash, "20260101T000000Z-0000abcd-link")
	os.Symlink(to, link)
	other := filepath.Join(trash, "not-a-trash-entry")
	os.Mkdir(other, 0o700)
	outside := filepath.Join(x.tmp, "20260101T000000Z-0000abcd-k")
	os.Mkdir(outside, 0o700)
	for name, c := range map[string]struct{ entry, dir string }{
		"a link in trash":                   {link, x.clone},
		"an entry the removal did not make": {other, x.clone},
		"an entry outside trash":            {outside, x.clone},
		"a place outside the workspaces":    {to, filepath.Join(x.tmp, "elsewhere")},
		"the workspaces directory itself":   {to, filepath.Join(x.root, Dir)},
	} {
		if err := x.r.Restore(ctx, c.entry, c.dir, nil); err == nil {
			t.Errorf("%s: restored", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(x.tmp, "elsewhere")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused restore made something outside the workspaces")
	}
	if got := treeSnapshot(t, to); fmt.Sprint(got) != fmt.Sprint(inside) {
		t.Fatal("a refused restore changed the entry in trash")
	}

	gone := filepath.Join(trash, "20260101T000000Z-0000abcd-gone")
	if err := x.r.Restore(ctx, gone, x.clone, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an entry the purge deleted: %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(x.clone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a restore of a gone entry put something in place")
	}
}

// A restore that comes while the purge runs waits for it: the purge never
// deletes an entry a restore took out halfway.
func TestRestoreWaitsForThePurge(t *testing.T) {
	x, to, inside := removed(t)
	trashMu.Lock()
	done := make(chan error, 1)
	go func() { done <- x.r.Restore(context.Background(), to, x.clone, nil) }()
	select {
	case err := <-done:
		trashMu.Unlock()
		t.Fatalf("the restore did not wait: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	trashMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := treeSnapshot(t, x.clone); fmt.Sprint(got) != fmt.Sprint(inside) {
		t.Fatal("the restored workspace's contents differ")
	}
}

// Remake clones the repository again at the workspace's place, and refuses,
// changing nothing, when anything is there or the place is outside the
// workspaces directory.
func TestRemakeClonesAgainAndOverwritesNothing(t *testing.T) {
	ctx := context.Background()
	x, to, _ := removed(t)
	os.RemoveAll(to) // as the purge does
	os.MkdirAll(x.clone, 0o700)
	os.WriteFile(filepath.Join(x.clone, "mine"), []byte("mine\n"), 0o600)
	there := treeSnapshot(t, x.clone)
	if err := x.r.Remake(ctx, 7, "file://"+x.bare, x.clone, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("remake over a directory: %v, want ErrExists", err)
	}
	if got := treeSnapshot(t, x.clone); fmt.Sprint(got) != fmt.Sprint(there) {
		t.Fatal("a refused remake changed the directory at the place")
	}
	if err := x.r.Remake(ctx, 7, "file://"+x.bare, filepath.Join(x.tmp, "elsewhere"), nil); err == nil {
		t.Fatal("a remake outside the workspaces directory was made")
	}
	if _, err := os.Lstat(filepath.Join(x.tmp, "elsewhere")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused remake made something outside the workspaces")
	}

	os.RemoveAll(x.clone)
	os.RemoveAll(filepath.Dir(x.clone)) // its parent too: the remake makes it
	if err := x.r.Remake(ctx, 7, "file://"+x.bare, x.clone, nil); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Check(ctx, x.clone); err != nil {
		t.Fatalf("the workspace made again is not a clean clone: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(x.clone, "a")); err != nil || string(b) != "one\n" {
		t.Fatalf("the clone's file: %q %v", b, err)
	}
}

// A directory between the workspaces directory and the place that is a
// link refuses the restore, and nothing is made where the link points.
func TestRestoreFollowsNoLinkToMakeItsParent(t *testing.T) {
	x, to, inside := removed(t)
	outside := filepath.Join(x.tmp, "outside")
	os.Mkdir(outside, 0o700)
	c := filepath.Join(x.root, Dir, "c")
	if err := os.RemoveAll(c); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, c); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Restore(context.Background(), to, x.clone, nil); err == nil {
		t.Fatal("restored through a link")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the restore made %v where the link points", entries)
	}
	if got := treeSnapshot(t, to); fmt.Sprint(got) != fmt.Sprint(inside) {
		t.Fatal("a refused restore changed the entry in trash")
	}
}

// Trashed finds the entry of a workspace at its thread's place by name, and
// no other's: not one whose name ends alike, not one the removal did not
// make, not that of a workspace elsewhere. Two entries are refused.
func TestTrashedFindsOnlyTheThreadsOwnEntry(t *testing.T) {
	root := t.TempDir()
	r := Remover{Maker: Maker{Root: root}}
	dir := Path(root, 7)
	if got, err := r.Trashed(7, dir); got != "" || err != nil {
		t.Fatalf("without trash: %q, %v", got, err)
	}
	mk := func(name string) string {
		p := filepath.Join(root, TrashDir, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("20260101T000000Z-0000abcd-17")
	mk("20260101T000000Z-0000abcd-x-7")
	mk("pending-20260101T000000Z-0000abcd-7")
	mk("7")
	if got, err := r.Trashed(7, dir); got != "" || err != nil {
		t.Fatalf("without an entry of thread 7: %q, %v", got, err)
	}
	want := mk("20260101T000000Z-0000abcd-7")
	if got, err := r.Trashed(7, dir); err != nil || !sameFile(t, got, want) {
		t.Fatalf("found %q, %v; want %s", got, err, want)
	}
	if got, err := r.Trashed(7, filepath.Join(root, Dir, "c", "s", "7")); got != "" || err != nil {
		t.Fatalf("for a place not named by the number: %q, %v", got, err)
	}
	mk("20260102T000000Z-0000abcd-7")
	if got, err := r.Trashed(7, dir); got != "" || err == nil {
		t.Fatalf("with two entries: %q, %v; want a refusal", got, err)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}
