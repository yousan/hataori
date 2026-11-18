package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// removal is an instance root with one workspace, a clone of a bare
// repository outside the root, all in the test's own temporary directory.
// The workspace starts clean and pushed: main with one commit, and a
// .gitignore that ignores *.log and cache/.
type removal struct {
	*repoFixture
	tmp  string
	root string
	r    Remover
}

func newRemoval(t *testing.T) *removal {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "root")
	f := &repoFixture{t: t, home: tmp, bare: filepath.Join(tmp, "remote", "repo.git"),
		clone: filepath.Join(root, Dir, "c", "s", "k")}
	work := filepath.Join(tmp, "work")
	for _, d := range []string{work, filepath.Dir(f.bare), filepath.Dir(f.clone)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.git(work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a"), []byte("one\n"), 0o600)
	os.WriteFile(filepath.Join(work, ".gitignore"), []byte("*.log\ncache/\n"), 0o600)
	f.git(work, "add", "a", ".gitignore")
	f.git(work, "commit", "-q", "-m", "first")
	f.git(tmp, "clone", "-q", "--bare", work, f.bare)
	f.git(tmp, "clone", "-q", f.bare, f.clone)
	return &removal{repoFixture: f, tmp: tmp, root: root,
		r: Remover{Maker: Maker{Root: root, Environ: f.env}, Timeout: time.Minute}}
}

func (x *removal) check() error {
	return x.r.Check(context.Background(), x.clone)
}

// commit commits a new file in dir.
func (x *removal) commit(dir, name string) {
	x.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
		x.t.Fatal(err)
	}
	x.git(dir, "add", name)
	x.git(dir, "commit", "-q", "-m", name)
}

// bareWithCommit makes another bare repository with one commit.
func (x *removal) bareWithCommit(name string) string {
	x.t.Helper()
	work := filepath.Join(x.tmp, name+"-work")
	os.MkdirAll(work, 0o700)
	x.git(work, "init", "-q", "-b", "main")
	x.commit(work, "s")
	bare := filepath.Join(x.tmp, "remote", name+".git")
	x.git(x.tmp, "clone", "-q", "--bare", work, bare)
	return bare
}

func wantRefusal(t *testing.T, err error, reason Reason, repo string) {
	t.Helper()
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("got %v, want a refusal for %q", err, reason)
	}
	if ref.Reason != reason || ref.Repo != repo {
		t.Fatalf("got %v (reason %q, repo %q), want reason %q in %q", err, ref.Reason, ref.Repo, reason, repo)
	}
	if !strings.Contains(err.Error(), string(reason)) {
		t.Fatalf("%v does not name the reason", err)
	}
}

func TestRemovableWhenCleanAndPushed(t *testing.T) {
	x := newRemoval(t)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseModifiedFile(t *testing.T) {
	x := newRemoval(t)
	x.write("a", "changed\n")
	wantRefusal(t, x.check(), Modified, "")
}

func TestRefuseStagedFile(t *testing.T) {
	x := newRemoval(t)
	x.write("b", "new\n")
	x.git(x.clone, "add", "b")
	wantRefusal(t, x.check(), Staged, "")
}

func TestRefuseUntrackedFile(t *testing.T) {
	x := newRemoval(t)
	x.write("b", "new\n")
	wantRefusal(t, x.check(), Untracked, "")
}

func TestRefuseHiddenFile(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "update-index", "--assume-unchanged", "a")
	x.write("a", "changed\n")
	wantRefusal(t, x.check(), Hidden, "")
}

func TestRefuseIgnoredFileUnlessDisposable(t *testing.T) {
	x := newRemoval(t)
	x.write("build.log", "x\n")
	wantRefusal(t, x.check(), Ignored, "")
	x.r.Disposable = []string{"*.log"}
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
	// A pattern with a slash is read from the top of the repository.
	os.MkdirAll(filepath.Join(x.clone, "cache", "deep"), 0o700)
	x.write("cache/deep/f", "x\n")
	x.r.Disposable = []string{"*.log", "other/cache"}
	wantRefusal(t, x.check(), Ignored, "")
	x.r.Disposable = []string{"*.log", "cache/deep"}
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseLinkedWorktree(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "worktree", "add", "-q", "--detach", filepath.Join(x.tmp, "linked"))
	wantRefusal(t, x.check(), Worktree, "")
}

func TestRefuseUnpushedCommitOnBranch(t *testing.T) {
	x := newRemoval(t)
	x.commit(x.clone, "b")
	wantRefusal(t, x.check(), UnpushedRef, "")
}

func TestRefuseDetachedHEADWithUnpushedCommit(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "checkout", "-q", "--detach")
	x.commit(x.clone, "b")
	wantRefusal(t, x.check(), DetachedHEAD, "")
}

// With reflogs off, the detached HEAD is the only thing naming the commit
// (rev-list --all includes HEAD).
func TestRefuseDetachedHEADWithoutReflog(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "config", "core.logAllRefUpdates", "false")
	if err := os.RemoveAll(filepath.Join(x.clone, ".git", "logs")); err != nil {
		t.Fatal(err)
	}
	x.git(x.clone, "checkout", "-q", "--detach")
	x.commit(x.clone, "b")
	if _, err := os.Lstat(filepath.Join(x.clone, ".git", "logs", "HEAD")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("HEAD has a reflog: %v", err)
	}
	wantRefusal(t, x.check(), DetachedHEAD, "")
}

func TestRefuseStash(t *testing.T) {
	x := newRemoval(t)
	x.write("a", "changed\n")
	x.git(x.clone, "stash", "-q")
	wantRefusal(t, x.check(), Stash, "")
}

func TestRefuseCommitOnlyInReflog(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "checkout", "-q", "-b", "gone")
	x.commit(x.clone, "b")
	x.git(x.clone, "checkout", "-q", "main")
	x.git(x.clone, "branch", "-q", "-D", "gone") // HEAD's reflog keeps it
	wantRefusal(t, x.check(), ReflogOnly, "")
}

// After a reset, ORIG_HEAD names the commit, also with reflogs off.
func TestRefuseCommitOnlyAtORIG_HEAD(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "config", "core.logAllRefUpdates", "false")
	if err := os.RemoveAll(filepath.Join(x.clone, ".git", "logs")); err != nil {
		t.Fatal(err)
	}
	x.commit(x.clone, "b")
	x.git(x.clone, "reset", "-q", "--hard", "HEAD~1")
	wantRefusal(t, x.check(), OtherRef, "")
}

// The remote-tracking ref says the commit is pushed; the remote no longer
// has it.
func TestRefuseCommitTheRemoteNoLongerHas(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "checkout", "-q", "-b", "feature")
	x.commit(x.clone, "b")
	x.git(x.clone, "push", "-q", "origin", "feature")
	x.git(x.clone, "checkout", "-q", "main")
	x.git(x.clone, "branch", "-q", "-D", "feature")
	x.git(x.bare, "branch", "-q", "-D", "feature")
	if st := x.mustState(); st.Unpushed != 0 {
		t.Fatalf("the last fetch should say pushed: %+v", st)
	}
	wantRefusal(t, x.check(), RemoteGone, "")
}

func TestRefuseSubmoduleWithUnpushedCommit(t *testing.T) {
	x := newRemoval(t)
	sub := x.bareWithCommit("sub")
	x.git(x.clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	x.git(x.clone, "commit", "-q", "-m", "add sub")
	x.git(x.clone, "push", "-q")
	if err := x.check(); err != nil {
		t.Fatalf("pushed submodule: %v", err)
	}
	x.commit(filepath.Join(x.clone, "sub"), "t")
	x.git(x.clone, "add", "sub")
	x.git(x.clone, "commit", "-q", "-m", "move sub")
	x.git(x.clone, "push", "-q")
	wantRefusal(t, x.check(), UnpushedRef, "sub")
}

func TestRefuseSubmoduleWithoutCheckout(t *testing.T) {
	x := newRemoval(t)
	sub := x.bareWithCommit("sub")
	x.git(x.clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	x.commit(filepath.Join(x.clone, "sub"), "t")
	x.git(x.clone, "add", "sub")
	x.git(x.clone, "commit", "-q", "-m", "add sub")
	x.git(x.clone, "push", "-q")
	x.git(x.clone, "submodule", "deinit", "-q", "-f", "sub")
	if _, err := os.Lstat(filepath.Join(x.clone, "sub", ".git")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("sub is still checked out: %v", err)
	}
	wantRefusal(t, x.check(), UnpushedRef, filepath.Join(".git", "modules", "sub"))
}

func TestRefuseNestedRepositoryWithUnpushedCommit(t *testing.T) {
	x := newRemoval(t)
	nested := filepath.Join(x.clone, "nested")
	os.Mkdir(nested, 0o700)
	x.git(nested, "init", "-q", "-b", "main")
	x.commit(nested, "n")
	x.git(x.clone, "add", "nested") // a gitlink with no .gitmodules
	x.git(x.clone, "commit", "-q", "-m", "nested")
	x.git(x.clone, "push", "-q")
	wantRefusal(t, x.check(), UnpushedRef, "nested")
}

// A bare repository in an ignored, disposable directory has no .git.
func TestRefuseNestedBareRepository(t *testing.T) {
	x := newRemoval(t)
	x.r.Disposable = []string{"cache"}
	os.Mkdir(filepath.Join(x.clone, "cache"), 0o700)
	own := filepath.Join(x.clone, "cache", "own.git")
	x.git(x.tmp, "init", "-q", "--bare", own)
	work := filepath.Join(x.tmp, "own-work")
	os.Mkdir(work, 0o700)
	x.git(work, "init", "-q", "-b", "main")
	x.commit(work, "o")
	x.git(work, "push", "-q", own, "main") // own.git has no remote
	wantRefusal(t, x.check(), UnpushedRef, filepath.Join("cache", "own.git"))
}

func TestRefuseRemoteThatIsAnotherWorkspace(t *testing.T) {
	x := newRemoval(t)
	other := filepath.Join(x.root, Dir, "c", "s", "other")
	x.git(x.tmp, "clone", "-q", x.bare, other)
	x.git(x.clone, "remote", "set-url", "origin", other)
	wantRefusal(t, x.check(), RemoteIsLocal, "")
}

func TestRefuseRemoteUnderAHataoriRoot(t *testing.T) {
	x := newRemoval(t)
	mirror := filepath.Join(x.root, "mirrors", "repo.git")
	x.git(x.tmp, "clone", "-q", "--bare", x.bare, mirror)
	x.git(x.clone, "remote", "set-url", "origin", "file://"+mirror)
	wantRefusal(t, x.check(), RemoteInRoot, "")

	// Under another instance's root.
	x.git(x.clone, "remote", "set-url", "origin", x.bare)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
	x.r.OtherRoots = []string{filepath.Dir(x.bare)}
	wantRefusal(t, x.check(), RemoteInRoot, "")
}

func TestRefuseProcessUsingTheDirectory(t *testing.T) {
	x := newRemoval(t)
	os.MkdirAll(filepath.Join(x.clone, "cache"), 0o700)
	x.r.Disposable = []string{"cache"}
	x.write("cache/f", "x\n")
	cmd := exec.Command("sleep", "60")
	cmd.Dir = filepath.Join(x.clone, "cache")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	wantRefusal(t, x.check(), InUse, "")
}

func TestRefuseGitError(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "remote", "set-url", "origin", filepath.Join(x.tmp, "missing.git"))
	wantRefusal(t, x.check(), GitError, "")
}

func TestRefuseTimeout(t *testing.T) {
	x := newRemoval(t)
	// A git daemon that accepts and never answers.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	x.git(x.clone, "remote", "set-url", "origin", "git://"+l.Addr().String()+"/repo.git")
	x.r.Timeout = time.Second
	start := time.Now()
	wantRefusal(t, x.check(), Timeout, "")
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the check took %v", d)
	}
}

func TestRefuseUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	x := newRemoval(t)
	x.r.Disposable = []string{"cache"}
	dir := filepath.Join(x.clone, "cache")
	os.Mkdir(dir, 0o700)
	x.write("cache/f", "x\n")
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	wantRefusal(t, x.check(), Unreadable, "")
}

func TestRefuseWhatIsNotAWorkspace(t *testing.T) {
	x := newRemoval(t)
	// A clean, pushed clone, removable but for where it is.
	outside := filepath.Join(x.tmp, "outside")
	x.git(x.tmp, "clone", "-q", x.bare, outside)
	for _, dir := range []string{outside, filepath.Join(x.root, Dir), x.root, filepath.Join(x.root, Dir, "c", "s", "missing")} {
		_, err := x.r.Remove(context.Background(), dir)
		wantRefusal(t, err, NotWorkspace, "")
	}
	link := filepath.Join(x.root, Dir, "c", "s", "link")
	if err := os.Symlink(x.clone, link); err != nil {
		t.Fatal(err)
	}
	_, err := x.r.Remove(context.Background(), link)
	wantRefusal(t, err, NotWorkspace, "")
}

// treeSnapshot is every path under dir with its mode and content, except
// under the skipped directories.
func treeSnapshot(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, s := range skip {
			if p == s {
				return filepath.SkipDir
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += fmt.Sprintf(" %x", sha256.Sum256(data))
		} else if !info.IsDir() {
			v += " " + info.ModTime().String()
		}
		rel, _ := filepath.Rel(dir, p)
		files[rel] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRemoveMovesIntoTrashAndNothingElse(t *testing.T) {
	x := newRemoval(t)
	x.r.Disposable = []string{"*.log"}
	x.write("build.log", "x\n")
	sibling := filepath.Join(x.root, Dir, "c", "s", "sibling")
	x.git(x.tmp, "clone", "-q", x.bare, sibling)
	before := treeSnapshot(t, x.tmp, x.clone)
	inside := treeSnapshot(t, x.clone)
	st, _ := os.Lstat(x.clone)
	dev, ino, _ := identity(st)

	to, err := x.r.Remove(context.Background(), x.clone)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(to) != filepath.Join(x.root, TrashDir) {
		t.Fatalf("moved to %s", to)
	}
	if _, err := os.Lstat(x.clone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the workspace is still there: %v", err)
	}
	st, err = os.Lstat(to)
	if err != nil {
		t.Fatal(err)
	}
	if d, i, _ := identity(st); d != dev || i != ino {
		t.Fatal("trash holds another directory, not the renamed one")
	}
	if got := treeSnapshot(t, to); fmt.Sprint(got) != fmt.Sprint(inside) {
		t.Fatal("the workspace's contents changed")
	}
	after := treeSnapshot(t, x.tmp, to, filepath.Join(x.root, scratchDir))
	// Only trash/ and the empty state/removal/ are new.
	delete(after, filepath.Join("root", TrashDir))
	delete(after, filepath.Join("root", scratchDir))
	delete(after, filepath.Join("root", "state"))
	if fmt.Sprint(after) != fmt.Sprint(before) {
		var diff []string
		for k, v := range after {
			if before[k] != v {
				diff = append(diff, "+"+k)
			}
		}
		for k, v := range before {
			if after[k] != v {
				diff = append(diff, "-"+k)
			}
		}
		sort.Strings(diff)
		t.Fatalf("something else changed: %v", diff)
	}
	if entries, _ := os.ReadDir(filepath.Join(x.root, scratchDir)); len(entries) != 0 {
		t.Fatalf("scratch repositories left: %v", entries)
	}
}

func TestRemoveRefusesAndLeavesTheWorkspace(t *testing.T) {
	x := newRemoval(t)
	x.write("b", "new\n")
	before := treeSnapshot(t, x.tmp, filepath.Join(x.root, scratchDir))
	_, err := x.r.Remove(context.Background(), x.clone)
	wantRefusal(t, err, Untracked, "")
	after := treeSnapshot(t, x.tmp, filepath.Join(x.root, scratchDir))
	delete(after, filepath.Join("root", scratchDir))
	delete(after, filepath.Join("root", "state"))
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("a refused removal changed something")
	}
}

func TestPurgeDeletesOnlyOldTrash(t *testing.T) {
	tmp := t.TempDir()
	trash := filepath.Join(tmp, TrashDir)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour).Format(trashStamp)
	recent := now.Add(-time.Hour).Format(trashStamp)
	for _, name := range []string{old + "-00000000-a", recent + "-00000000-b", "not-from-removal", "2026-c", "XXXXXXXXXXXXXXXX-d", old + "-backup", "20261399T000000Z-00000000-e"} {
		os.MkdirAll(filepath.Join(trash, name, "d"), 0o700)
		os.WriteFile(filepath.Join(trash, name, "d", "f"), []byte("x"), 0o600)
	}
	os.WriteFile(filepath.Join(tmp, "outside"), []byte("x"), 0o600)
	r := Remover{Maker: Maker{Root: tmp}, Now: func() time.Time { return now }}
	if _, err := r.Purge(); err == nil {
		t.Fatal("purge ran with no retention")
	}
	r.Retention = 24 * time.Hour
	purged, err := r.Purge()
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || filepath.Base(purged[0]) != old+"-00000000-a" {
		t.Fatalf("purged %v", purged)
	}
	var left []string
	entries, _ := os.ReadDir(trash)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"2026-c", old + "-backup", recent + "-00000000-b", "20261399T000000Z-00000000-e", "XXXXXXXXXXXXXXXX-d", "not-from-removal"}; fmt.Sprint(left) != fmt.Sprint(want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	if _, err := os.Stat(filepath.Join(tmp, "outside")); err != nil {
		t.Fatal(err)
	}
}

// A trash/ that is a link leads out of the root: nothing is moved there,
// and nothing there is purged.
func TestTrashThatIsALinkIsRefused(t *testing.T) {
	x := newRemoval(t)
	elsewhere := filepath.Join(x.tmp, "elsewhere")
	old := filepath.Join(elsewhere, "20200101T000000Z-00000000-x")
	os.MkdirAll(old, 0o700)
	if err := os.Symlink(elsewhere, filepath.Join(x.root, TrashDir)); err != nil {
		t.Fatal(err)
	}
	_, err := x.r.Remove(context.Background(), x.clone)
	wantRefusal(t, err, NotWorkspace, "")
	if _, err := os.Lstat(x.clone); err != nil {
		t.Fatal(err)
	}
	x.r.Retention = time.Hour
	if _, err := x.r.Purge(); err == nil {
		t.Fatal("purge followed the link")
	}
	if _, err := os.Lstat(old); err != nil {
		t.Fatal(err)
	}
}
