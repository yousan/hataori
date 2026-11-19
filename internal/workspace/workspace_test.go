package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// bareRepo makes a bare repository with one commit, in a new temporary
// directory, and returns its file:// address.
func bareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work, bare := filepath.Join(dir, "work"), filepath.Join(dir, "repo.git")
	git := func(cwd string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.Mkdir(work, 0o700)
	git(work, "init", "-q")
	os.WriteFile(filepath.Join(work, "README"), []byte("hello\n"), 0o600)
	git(work, "add", "README")
	git(work, "commit", "-q", "-m", "first")
	git(dir, "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

func TestPathSegmentsAreSafe(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("k", 300)
	keys := []string{"..", ".", "a/b", "../../etc", "", long, long + "x", "%", "~", "plain-key_1.2"}
	seen := map[string]string{}
	for _, k := range keys {
		p := filepath.Join(root, Dir, Segment("chat"), Segment("scope/1"), Segment(k))
		rel, err := filepath.Rel(filepath.Join(root, Dir), p)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("key %.20q: %s is outside workspaces", k, p)
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 || parts[0] != "chat" || parts[1] != "scope%2F1" {
			t.Errorf("key %.20q: %s is not <client>/<scope>/<key>", k, rel)
		}
		if len(parts[2]) > maxSegment || parts[2] == "." || parts[2] == ".." || parts[2] == "" {
			t.Errorf("key %.20q: segment %q", k, parts[2])
		}
		if other, ok := seen[p]; ok {
			t.Errorf("keys %.20q and %.20q share %s", k, other, p)
		}
		seen[p] = k
	}
	if Segment("plain-key_1.2") != "plain-key_1.2" {
		t.Error("a safe key was changed")
	}
}

func TestCloneIntoPlace(t *testing.T) {
	root := t.TempDir()
	m := Maker{Root: root}
	dir, err := m.Clone(context.Background(), 1, bareRepo(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != Path(root, 1) {
		t.Fatalf("cloned at %s", dir)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "README")); err != nil || string(b) != "hello\n" {
		t.Fatalf("clone content: %q, %v", b, err)
	}
}

// An empty directory that appears while the clone runs is not replaced.
func TestRenameNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	os.Mkdir(from, 0o700)
	os.Mkdir(to, 0o700)
	if err := renameNoReplace(from, to); !errors.Is(err, os.ErrExist) {
		t.Fatalf("rename onto an empty directory: %v", err)
	}
	if _, err := os.Stat(from); err != nil {
		t.Fatal("the source moved")
	}
}

func TestExistingDirectoryIsUntouched(t *testing.T) {
	root := t.TempDir()
	target := Path(root, 1)
	os.MkdirAll(target, 0o700)
	os.WriteFile(filepath.Join(target, "work.txt"), []byte("mine"), 0o600)
	_, err := Maker{Root: root}.Clone(context.Background(), 1, bareRepo(t), nil)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("clone over an existing directory: %v", err)
	}
	entries, _ := os.ReadDir(target)
	if b, _ := os.ReadFile(filepath.Join(target, "work.txt")); len(entries) != 1 || string(b) != "mine" {
		t.Fatalf("the directory changed: %v", entries)
	}
}

func TestBadAddressesRefusedBeforeGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no git: any attempt to run it would fail differently
	root := t.TempDir()
	for _, repo := range []string{"host/owner/repo", "git@host/owner/repo", "", " https://h/r", "ftp://host/repo",
		"https://", "https://host", "-upload-pack=x", "owner/repo"} {
		_, err := Maker{Root: root}.Clone(context.Background(), 1, repo, nil)
		if err == nil || !strings.Contains(err.Error(), "repository address") {
			t.Errorf("%q: %v", repo, err)
		}
	}
	for _, repo := range []string{"https://host/owner/repo.git", "ssh://git@host/owner/repo", "git@host:owner/repo.git",
		"host:owner/repo.git", "git@host:/srv/repo.git", "file:///srv/repo.git"} {
		if err := CheckAddress(repo); err != nil {
			t.Errorf("%q refused: %v", repo, err)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused address left %v", entries)
	}
}

// git is wrapped by a script that records where it was asked to clone.
func TestCloneIsMadeOutsideWorkspaces(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "args")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$@\" >> '"+log+"'\nexec '"+real+"' \"$@\"\n"), 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	repo := bareRepo(t)
	if _, err := (Maker{Root: root}).Clone(context.Background(), 1, repo, nil); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), filepath.Join(root, cloneDir)) || strings.Contains(string(args), filepath.Join(root, Dir)) {
		t.Fatalf("git was run as %q", args)
	}

	// A clone that fails never appears under workspaces/.
	_, err = Maker{Root: root}.Clone(context.Background(), 2, "file://"+filepath.Join(t.TempDir(), "missing.git"), nil)
	if err == nil {
		t.Fatal("a clone of a missing repository succeeded")
	}
	if _, err := os.Lstat(Path(root, 2)); !os.IsNotExist(err) {
		t.Fatalf("a failed clone appeared: %v", err)
	}
}

func TestFailedCloneHidesCredentials(t *testing.T) {
	// A server of the test's own on the loopback address; nothing leaves the host.
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	addr := strings.Replace(srv.URL, "http://", "http://someone:s3cret-pass@", 1) + "/repo.git"
	m := Maker{Root: t.TempDir(), Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()} }}
	_, err := m.Clone(context.Background(), 1, addr, nil)
	if err == nil {
		t.Fatal("clone succeeded")
	}
	if strings.Contains(err.Error(), "s3cret-pass") || !strings.Contains(err.Error(), "***@") {
		t.Fatalf("error %q", err)
	}
	if !strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("git's own last lines are missing: %q", err)
	}
}

func TestGitIgnoresTheDaemonsGitVariables(t *testing.T) {
	repo := bareRepo(t)
	elsewhere := t.TempDir()
	t.Setenv("GIT_DIR", elsewhere)
	t.Setenv("GIT_WORK_TREE", elsewhere)
	t.Setenv("GIT_COMMON_DIR", elsewhere)
	root := t.TempDir()
	dir, err := Maker{Root: root}.Clone(context.Background(), 1, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("no repository in the workspace: %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("git wrote into GIT_DIR: %v", entries)
	}
}

var _ core.Workspaces = Maker{}

// Adopt takes the directory a clone placed, and refuses another one at the
// same place without touching it.
func TestAdoptOnlyTheDirectoryItPlaced(t *testing.T) {
	root := t.TempDir()
	m := Maker{Root: root}
	th := control.ThreadID(1)
	var p core.Placement
	dir, err := m.Make(context.Background(), th, bareRepo(t), func(got core.Placement) error { p = got; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := m.Adopt(context.Background(), th, p); err != nil || !ok || got != dir {
		t.Fatalf("adopt the placed clone: %q, %v, %v", got, ok, err)
	}
	// Another directory at the same place.
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "theirs.txt"), []byte("theirs"), 0o600)
	if _, ok, err := m.Adopt(context.Background(), th, p); ok || !errors.Is(err, ErrExists) {
		t.Fatalf("adopt another directory: %v, %v", ok, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "theirs.txt")); string(b) != "theirs" {
		t.Fatal("the other directory was touched")
	}
}

// A clone a release before thread numbers placed, at its own path, is
// adopted there by its device and inode; a placement outside the
// workspaces directory is refused.
func TestAdoptAPlacementOfTheReleaseBefore(t *testing.T) {
	root := t.TempDir()
	m := Maker{Root: root}
	old := filepath.Join(root, Dir, "chat", "c1", "t1")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(old)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, _ := identity(st)
	got, ok, err := m.Adopt(context.Background(), 1, core.Placement{Dir: old, Dev: dev, Inode: ino})
	if err != nil || !ok || got != old {
		t.Fatalf("adopt the old placement: %q, %v, %v", got, ok, err)
	}
	if _, ok, err := m.Adopt(context.Background(), 1, core.Placement{Dir: old, Dev: dev, Inode: ino + 1}); ok || !errors.Is(err, ErrExists) {
		t.Fatalf("another directory at the old place: %v, %v", ok, err)
	}
	outside := t.TempDir()
	for _, dir := range []string{outside, filepath.Join(root, Dir), filepath.Join(root, Dir, "..", "state")} {
		if _, ok, err := m.Adopt(context.Background(), 1, core.Placement{Dir: dir, Dev: dev, Inode: ino}); ok || err == nil {
			t.Fatalf("a placement at %s: %v, %v", dir, ok, err)
		}
	}
}

// Empty makes a confined thread's directory empty, outside the workspaces
// directory, and takes again only an empty directory of its own at its
// place: a directory with something in it, or a link at its place or at
// confined/, is refused and left exactly as it is.
func TestEmptyNeverTakesWhatHoldsSomething(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	m := Maker{Root: root}
	dir, err := m.Empty(ctx, 1)
	if err != nil || dir != filepath.Join(root, ConfinedDir, "1") {
		t.Fatalf("Empty: %q, %v", dir, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the directory: %v, %v", entries, err)
	}
	if rel, _ := filepath.Rel(filepath.Join(root, Dir), dir); !strings.HasPrefix(rel, "..") {
		t.Fatalf("%s is inside the workspaces directory", dir)
	}
	// Made before a daemon died, and still empty: taken again.
	if again, err := m.Empty(ctx, 1); err != nil || again != dir {
		t.Fatalf("Empty again: %q, %v", again, err)
	}
	// Someone's file in it: refused, and the file stays.
	notes := filepath.Join(dir, "notes.txt")
	os.WriteFile(notes, []byte("work\n"), 0o600)
	if _, err := m.Empty(ctx, 1); !errors.Is(err, ErrExists) {
		t.Fatalf("a directory with a file: %v, want refused", err)
	}
	if b, err := os.ReadFile(notes); err != nil || string(b) != "work\n" {
		t.Fatalf("the file after the refusal: %q, %v", b, err)
	}
	// A link at its place, to an empty directory elsewhere: refused.
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, ConfinedDir, "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Empty(ctx, 2); err == nil {
		t.Fatal("a link at the directory's place was taken")
	}
	// confined/ itself a link: refused, and nothing is made where it points.
	other := Maker{Root: t.TempDir()}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(other.Root, ConfinedDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Empty(ctx, 1); err == nil {
		t.Fatal("a link in place of confined/ was followed")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("Empty made %v where the link points", entries)
	}
}
