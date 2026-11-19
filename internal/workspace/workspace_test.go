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
