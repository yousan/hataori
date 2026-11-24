package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
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
	return newRemovalIn(t, "root")
}

// newRemovalIn makes the instance root a directory of that name.
func newRemovalIn(t *testing.T, name string) *removal {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, name)
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
	x.commitAs(dir, name, name+"\n")
}

// commitAs commits a new file with a content in dir.
func (x *removal) commitAs(dir, name, content string) {
	x.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
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

// autostashed makes the workspace hold uncommitted work that only an
// autostash of a stopped rebase, merge or checkout holds, and returns that commit. The
// tree is clean, and every commit a ref or reflog names is pushed.
func (x *removal) autostashed(kind string) string {
	x.t.Helper()
	x.git(x.clone, "branch", "-q", "other")
	x.commit(x.clone, "b")
	x.git(x.clone, "checkout", "-q", "other")
	x.commitAs(x.clone, "b", "another\n") // conflicts with main's
	x.git(x.clone, "checkout", "-q", "main")
	x.git(x.clone, "push", "-q", "origin", "main", "other")
	if err := x.check(); err != nil {
		x.t.Fatalf("clean and pushed: %v", err)
	}
	x.write("a", "work that is committed nowhere\n")
	// Those that stop exit with 1; what they leave is looked at below.
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = x.clone, x.env()
		out, err := cmd.CombinedOutput()
		x.t.Logf("git %v: %v\n%s", args, err, out)
	}
	switch kind {
	case "rebase-merge/autostash":
		run("rebase", "--autostash", "--exec", "false", "HEAD~1")
	case "rebase-apply/autostash":
		run("rebase", "--apply", "--autostash", "other")
		x.git(x.clone, "reset", "-q", "--hard") // the rebase goes on
	case "MERGE_AUTOSTASH":
		run("merge", "-s", "ours", "--no-commit", "--autostash", "other")
	case "CHECKOUT_AUTOSTASH_HEAD":
		// What checkout -m of git 2.56 leaves when it dies, as on a failing
		// required filter; git before it has no such autostash.
		x.git(x.clone, "update-ref", kind, x.git(x.clone, "stash", "create"))
		x.git(x.clone, "reset", "-q", "--hard")
	}
	b, err := os.ReadFile(filepath.Join(x.clone, ".git", filepath.FromSlash(kind)))
	if err != nil {
		x.t.Fatalf("no autostash: %v", err)
	}
	id := strings.TrimSpace(string(b))
	if got := x.git(x.clone, "status", "--porcelain", "--untracked-files=all"); got != "" {
		x.t.Fatalf("the tree is not clean:\n%s", got)
	}
	if got := x.git(x.clone, "stash", "list"); got != "" {
		x.t.Fatalf("the stash is not empty:\n%s", got)
	}
	if got := x.git(x.clone, "show", id+":a"); got != "work that is committed nowhere" {
		x.t.Fatalf("the autostash %s holds %q", id, got)
	}
	return id
}

func TestRefuseAutostash(t *testing.T) {
	for _, kind := range []string{"rebase-merge/autostash", "rebase-apply/autostash", "MERGE_AUTOSTASH", "CHECKOUT_AUTOSTASH_HEAD"} {
		t.Run(kind, func(t *testing.T) {
			x := newRemoval(t)
			id := x.autostashed(kind)
			err := x.check()
			wantRefusal(t, err, Stash, "")
			if d := err.(*Refusal).Detail; d != "commit "+id {
				t.Fatalf("the refusal names %q, want commit %s", d, id)
			}

			// A remote that has the commit is a place it exists.
			x.git(x.clone, "push", "-q", "origin", id+":refs/heads/wip")
			if err := x.check(); err != nil {
				t.Fatalf("the autostash is pushed: %v", err)
			}

			// What names no commit answers nothing.
			p := filepath.Join(x.clone, ".git", filepath.FromSlash(kind))
			tree := x.git(x.clone, "rev-parse", "HEAD^{tree}")
			for _, content := range []string{"", "HEAD\n", id[:20] + "\n", strings.Repeat("1", 40) + "\n", tree + "\n", id + "\n" + id + "\n"} {
				if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := x.check(); err == nil {
					t.Fatalf("the check passed with %q in %s", content, kind)
				}
			}
			// Nor does what is no file of git's: a link that leads nowhere, or a
			// FIFO, which would hold up the one that reads it.
			os.Remove(p)
			if err := os.Symlink("gone", p); err != nil {
				t.Fatal(err)
			}
			if err := x.check(); err == nil {
				t.Fatalf("the check passed with a dangling link at %s", kind)
			}
			os.Remove(p)
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- x.check() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("the check passed with a FIFO at %s", kind)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("the check hangs at a FIFO at %s", kind)
			}

			// Nor does what cannot be looked at.
			if dir := filepath.Dir(p); strings.Contains(kind, "/") {
				os.RemoveAll(dir)
				if err := os.WriteFile(dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := x.check(); err == nil {
					t.Fatalf("the check passed with a file at %s", dir)
				}
			}
		})
	}
}

// The git directory of a submodule is checked without its work tree too.
func TestRefuseAutostashOfASubmoduleWithoutCheckout(t *testing.T) {
	x := newRemoval(t)
	sub := x.bareWithCommit("sub")
	x.git(x.clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	x.commit(filepath.Join(x.clone, "sub"), "t")
	x.git(filepath.Join(x.clone, "sub"), "push", "-q")
	x.git(x.clone, "add", "sub")
	x.git(x.clone, "commit", "-q", "-m", "add sub")
	x.git(x.clone, "push", "-q")
	x.write("sub/s", "work that is committed nowhere\n")
	cmd := exec.Command("git", "rebase", "--autostash", "--exec", "false", "HEAD~1")
	cmd.Dir, cmd.Env = filepath.Join(x.clone, "sub"), x.env()
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the rebase did not stop:\n%s", out)
	}
	x.git(x.clone, "submodule", "deinit", "-q", "-f", "sub")
	wantRefusal(t, x.check(), Stash, filepath.Join(".git", "modules", "sub"))
}

// With its refs in a reftable, a repository has no file for a pseudo-ref; a
// file there is none of git's, and says nothing about the pseudo-ref.
func TestRefuseAutostashInAReftable(t *testing.T) {
	for _, ref := range []string{"MERGE_AUTOSTASH", "CHECKOUT_AUTOSTASH_HEAD"} {
		t.Run(ref, func(t *testing.T) {
			x := newRemoval(t)
			os.RemoveAll(x.clone)
			cmd := exec.Command("git", "clone", "-q", "--ref-format=reftable", x.bare, x.clone)
			cmd.Env = x.env()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("this git makes no reftable: %.60s", out)
			}
			x.commit(x.clone, "b")
			x.git(x.clone, "push", "-q", "origin", "main")
			if err := x.check(); err != nil {
				t.Fatal(err)
			}
			x.git(x.clone, "branch", "-q", "other", "HEAD~1")
			x.git(x.clone, "checkout", "-q", "other")
			x.commit(x.clone, "c")
			x.git(x.clone, "push", "-q", "origin", "other")
			x.git(x.clone, "checkout", "-q", "main")
			x.write("a", "work that is committed nowhere\n")
			if ref == "MERGE_AUTOSTASH" {
				x.git(x.clone, "merge", "-q", "-s", "ours", "--no-commit", "--autostash", "other")
			} else {
				x.git(x.clone, "update-ref", ref, x.git(x.clone, "stash", "create"))
				x.git(x.clone, "reset", "-q", "--hard")
			}
			x.git(x.clone, "rev-parse", "--verify", ref)
			p := filepath.Join(x.clone, ".git", ref)
			if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s is a file: %v", ref, err)
			}
			wantRefusal(t, x.check(), Stash, "")
			if err := os.WriteFile(p, []byte(x.git(x.clone, "rev-parse", "HEAD")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, x.check(), Stash, "")
		})
	}
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

// withSubmodule adds a submodule at sub, with everything pushed, and returns
// its directory.
func (x *removal) withSubmodule() string {
	x.t.Helper()
	sub := x.bareWithCommit("sub")
	x.git(x.clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	x.git(x.clone, "commit", "-q", "-m", "add sub")
	x.git(x.clone, "push", "-q")
	if err := x.check(); err != nil {
		x.t.Fatalf("pushed submodule: %v", err)
	}
	return filepath.Join(x.clone, "sub")
}

// withoutCheckout are the ways git removes the checkout of the submodule at
// sub and keeps its git directory. After git rm, core.worktree names a
// directory that is gone, and git runs nothing there; after the others it is
// not set.
var withoutCheckout = map[string]func(x *removal){
	"git rm": func(x *removal) {
		x.git(x.clone, "rm", "-q", "-f", "sub")
		x.git(x.clone, "commit", "-q", "-m", "remove sub")
		x.git(x.clone, "push", "-q")
		if got := x.git(x.clone, "config", "--file", ".git/modules/sub/config", "core.worktree"); got != "../../../sub" {
			x.t.Fatalf("core.worktree is %q", got)
		}
	},
	"deinit": func(x *removal) {
		x.git(x.clone, "submodule", "deinit", "-q", "-f", "sub")
	},
	"checkout with submodule.recurse": func(x *removal) {
		x.git(x.clone, "-c", "submodule.recurse=true", "checkout", "-q", "-f", "HEAD^{/first}")
	},
}

func TestRemovableAfterASubmodulesCheckoutWasRemoved(t *testing.T) {
	for name, remove := range withoutCheckout {
		t.Run(name, func(t *testing.T) {
			x := newRemoval(t)
			sub := x.withSubmodule()
			remove(x)
			if _, err := os.Lstat(filepath.Join(sub, ".git")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("sub is still checked out: %v", err)
			}
			if err := x.check(); err != nil {
				t.Fatalf("nothing would be lost, and the check refuses: %v", err)
			}
			if entries, err := os.ReadDir(filepath.Join(x.root, scratchDir)); err != nil || len(entries) != 0 {
				t.Fatalf("the check leaves %v, %v", entries, err)
			}
		})
	}
}

// What is only in the git directory refuses, whatever removed the checkout.
func TestRefuseWhatASubmoduleWithoutCheckoutHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		reason Reason
		make   func(x *removal, sub string)
	}{
		"commit": {UnpushedRef, func(x *removal, sub string) {
			x.commit(sub, "t")
			x.git(sub, "push", "-q", "origin", "main")
			x.git(x.clone, "add", "sub")
			x.git(x.clone, "commit", "-q", "-m", "move sub")
			x.git(x.clone, "push", "-q")
			x.git(filepath.Join(x.tmp, "remote", "sub.git"), "update-ref", "refs/heads/main", "main~1")
		}},
		"linked worktree": {Worktree, func(x *removal, sub string) {
			x.git(sub, "worktree", "add", "-q", "--detach", filepath.Join(x.tmp, "linked"))
		}},
	} {
		for how, remove := range withoutCheckout {
			t.Run(name+" after "+how, func(t *testing.T) {
				x := newRemoval(t)
				sub := x.withSubmodule()
				tc.make(x, sub)
				remove(x)
				wantRefusal(t, x.check(), tc.reason, filepath.Join(".git", "modules", "sub"))
			})
		}
	}
	// git takes the file out of the index with submodule.recurse.
	for _, how := range []string{"git rm", "deinit"} {
		for name, stage := range map[string]func(x *removal, sub string){
			"new file": func(x *removal, sub string) {
				x.write("sub/new", "work that is committed nowhere\n")
				x.git(sub, "add", "new")
			},
			"changed file": func(x *removal, sub string) {
				x.write("sub/s", "work that is committed nowhere\n")
				x.git(sub, "add", "s")
			},
			"renamed file": func(x *removal, sub string) {
				x.git(sub, "mv", "s", "moved")
			},
			"file before the first commit": func(x *removal, sub string) {
				x.git(sub, "checkout", "-q", "--orphan", "fresh")
			},
		} {
			t.Run(name+" staged after "+how, func(t *testing.T) {
				x := newRemoval(t)
				stage(x, x.withSubmodule())
				withoutCheckout[how](x)
				wantRefusal(t, x.check(), Staged, filepath.Join(".git", "modules", "sub"))
			})
		}
	}
}

// A linked worktree of a bare repository is elsewhere, and needs it.
func TestRefuseLinkedWorktreeOfANestedBareRepository(t *testing.T) {
	x := newRemoval(t)
	x.r.Disposable = []string{"cache"}
	os.Mkdir(filepath.Join(x.clone, "cache"), 0o700)
	bare := filepath.Join(x.clone, "cache", "b.git")
	x.git(x.tmp, "clone", "-q", "--bare", x.bare, bare)
	if err := x.check(); err != nil {
		t.Fatalf("pushed bare repository: %v", err)
	}
	x.git(bare, "worktree", "add", "-q", "--detach", filepath.Join(x.tmp, "linked"))
	wantRefusal(t, x.check(), Worktree, filepath.Join("cache", "b.git"))
}

// A plain clone, which is what Maker.Clone makes, leaves the directory of a
// submodule empty and no repository. git reports nothing below it: not as
// untracked, not as ignored.
func TestRefuseAnythingInTheDirectoryOfASubmoduleNotCheckedOut(t *testing.T) {
	x := newRemoval(t)
	sub := x.bareWithCommit("sub")
	for _, p := range []string{"sub", "deps/a b\tü"} {
		x.git(x.clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", strconv.Itoa(len(p)), sub, p)
	}
	x.git(x.clone, "commit", "-q", "-m", "add sub")
	x.git(x.clone, "push", "-q")
	if err := x.check(); err != nil {
		t.Fatalf("every submodule is checked out: %v", err)
	}

	m := Maker{Root: x.root, Environ: x.env}
	ws, err := m.Clone(context.Background(), 7, "file://"+x.bare, nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func() error { return x.r.Check(context.Background(), ws) }
	put := func(name string) {
		t.Helper()
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("work that is committed nowhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		put  func()
		want string // what the refusal names
	}{
		{"a file", func() { put("sub/notes.txt") }, "sub/notes.txt"},
		{"a file in a directory", func() { put("sub/d/e/notes.txt") }, "sub/d/e/notes.txt"},
		{"a file after an empty directory", func() {
			os.MkdirAll(filepath.Join(ws, "sub", "a"), 0o700)
			put("sub/b/notes.txt")
		}, "sub/b/notes.txt"},
		{"an ignored, disposable name", func() { put("sub/x.log") }, "sub/x.log"},
		{"a directory", func() { os.MkdirAll(filepath.Join(ws, "sub", "d"), 0o700) }, "sub/d"},
		{"a link", func() { os.Symlink("nowhere", filepath.Join(ws, "sub", "l")) }, "sub/l"},
		{"below a path git quotes", func() { put("deps/a b\tü/notes.txt") }, "deps/a b\tü/notes.txt"},
		// find takes it for a repository, but it has no work tree whose
		// files are checked.
		{"a bare repository", func() {
			x.git(x.tmp, "clone", "-q", "--bare", sub, filepath.Join(ws, "sub"))
			put("sub/notes.txt")
		}, "sub/HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x.r.Disposable = []string{"*.log"}
			if err := check(); err != nil {
				t.Fatalf("a new workspace: %v", err)
			}
			t.Cleanup(func() {
				for _, p := range []string{"sub", "deps/a b\tü"} {
					dir := filepath.Join(ws, p)
					os.RemoveAll(dir)
					os.Mkdir(dir, 0o700)
				}
			})
			tc.put()
			if got := x.git(ws, "status", "--porcelain", "--untracked-files=all", "--ignored"); got != "" {
				t.Fatalf("git status reports it after all:\n%s", got)
			}
			err := check()
			wantRefusal(t, err, Untracked, "")
			if d := err.(*Refusal).Detail; d != tc.want {
				t.Fatalf("the refusal names %q, want %q", d, tc.want)
			}
		})
	}
}

// The same one level down: a submodule that is checked out, but not its own.
func TestRefuseAnythingInTheDirectoryOfANestedSubmoduleNotCheckedOut(t *testing.T) {
	x := newRemoval(t)
	inner := x.bareWithCommit("inner")
	outer := x.bareWithCommit("outer")
	work := filepath.Join(x.tmp, "outer-work")
	x.git(work, "-c", "protocol.file.allow=always", "submodule", "add", "-q", inner, "in")
	x.git(work, "commit", "-q", "-m", "add in")
	x.git(work, "push", "-q", outer, "main")
	x.git(x.clone, "-c", "protocol.file.allow=always", "submodule", "add", "-q", outer, "out")
	x.git(x.clone, "commit", "-q", "-m", "add out")
	x.git(x.clone, "push", "-q")
	if err := x.check(); err != nil {
		t.Fatalf("untouched: %v", err)
	}
	x.write("out/in/notes.txt", "work that is committed nowhere\n")
	err := x.check()
	wantRefusal(t, err, Untracked, "out")
	if d := err.(*Refusal).Detail; d != "in/notes.txt" {
		t.Fatalf("the refusal names %q", d)
	}
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

func (x *removal) symlink(target, name string) string {
	x.t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		x.t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		x.t.Fatal(err)
	}
	return name
}

// A local remote address is resolved the way git resolves it. Each
// address below leads git to the sibling workspace's repository, at
// <root>/workspaces/c/s/other/b.git, or cannot be resolved, so it does not
// count.
func TestRefuseLocalRemoteResolvedLikeGit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason Reason
		addr   func(x *removal, b string) string
	}{
		// ".." after a symlink goes up from the link's target. As text
		// this is <tmp>/l/root/..., outside the root.
		{"dot-dot after a symlink", RemoteIsLocal, func(x *removal, b string) string {
			return x.dotDotLink() + "/../root/" + Dir + "/c/s/other/b.git"
		}},
		{"file URL with dot-dot after a symlink", RemoteIsLocal, func(x *removal, b string) string {
			return "file://" + x.dotDotLink() + "/../root/" + Dir + "/c/s/other/b.git"
		}},
		// git tries <path>.git when <path> does not exist.
		{"name without .git", RemoteIsLocal, func(x *removal, b string) string {
			return x.symlink(filepath.Dir(b), filepath.Join(x.tmp, "link2")) + "/b"
		}},
		// git follows a .git file, in a directory or as the path itself.
		{"directory with a .git file", RemoteIsLocal, func(x *removal, b string) string {
			g := filepath.Join(x.tmp, "g")
			os.Mkdir(g, 0o700)
			os.WriteFile(filepath.Join(g, ".git"), []byte("gitdir: "+b+"\n"), 0o600)
			return g
		}},
		{"relative .git file", RemoteIsLocal, func(x *removal, b string) string {
			g := filepath.Join(x.tmp, "g")
			os.Mkdir(g, 0o700)
			os.WriteFile(filepath.Join(g, ".git"), []byte("gitdir: ../root/"+Dir+"/c/s/other/b.git\n"), 0o600)
			return g
		}},
		{"a .git file", RemoteIsLocal, func(x *removal, b string) string {
			g := filepath.Join(x.tmp, "gitfile")
			os.WriteFile(g, []byte("gitdir: "+b+"\n"), 0o600)
			return g
		}},
		// A linked worktree's git directory reads the refs and objects
		// of the common directory it names.
		{"worktree git directory", RemoteIsLocal, func(x *removal, b string) string {
			return x.worktreeGitDir(b)
		}},
		{"worktree git directory, relative", RemoteIsLocal, func(x *removal, b string) string {
			return x.worktreeGitDir("../root/" + Dir + "/c/s/other/b.git")
		}},
		// git expands a leading ~/ to $HOME, which is x.tmp.
		{"home", RemoteIsLocal, func(x *removal, b string) string {
			return "~/root/" + Dir + "/c/s/other/b.git"
		}},
		{"another user's home", RemoteUnresolved, func(x *removal, b string) string {
			return "~nobody/root/" + Dir + "/c/s/other/b.git"
		}},
		{"symlink loop", RemoteUnresolved, func(x *removal, b string) string {
			loop := filepath.Join(x.tmp, "loop")
			return x.symlink(loop, loop) + "/b.git"
		}},
		{"undecodable file URL", RemoteUnresolved, func(x *removal, b string) string {
			return "file://" + x.tmp + "/%zz"
		}},
		// upload-pack drops trailing slashes before it tries <path>.git.
		{"trailing slash", RemoteIsLocal, func(x *removal, b string) string {
			return x.symlink(filepath.Dir(b), filepath.Join(x.tmp, "link2")) + "/b/"
		}},
		// git decodes the whole URL before it takes the host: the path
		// starts at the decoded slash.
		{"file URL with an encoded slash in the host", RemoteIsLocal, func(x *removal, b string) string {
			return "file://x%2F" + b[1:]
		}},
		// Nothing there that git would read, or a .git file naming
		// nothing: what git does instead cannot be told.
		{"nothing at the address", RemoteUnresolved, func(x *removal, b string) string {
			return filepath.Join(x.tmp, "missing.git")
		}},
		{"empty .git file", RemoteUnresolved, func(x *removal, b string) string {
			g := filepath.Join(x.tmp, "gitfile")
			os.WriteFile(g, []byte("gitdir: \n"), 0o600)
			return g
		}},
		{".git file naming nothing", RemoteUnresolved, func(x *removal, b string) string {
			g := filepath.Join(x.tmp, "gitfile")
			os.WriteFile(g, []byte("gitdir: "+filepath.Join(x.tmp, "missing.git")+"\n"), 0o600)
			return g
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRemoval(t)
			b := filepath.Join(x.root, Dir, "c", "s", "other", "b.git")
			x.git(x.tmp, "clone", "-q", "--bare", x.bare, b)
			x.git(x.clone, "remote", "set-url", "origin", tc.addr(x, b))
			wantRefusal(t, x.check(), tc.reason, "")
		})
	}
}

// A ~/ address outside every root counts: it is $HOME's.
func TestRemoteByHomeOutsideTheRootCounts(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "remote", "set-url", "origin", "~/remote/repo.git") // HOME is x.tmp
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

// worktreeGitDir is <tmp>/wt, a linked worktree's git directory whose
// commondir file holds common.
func (x *removal) worktreeGitDir(common string) string {
	d := filepath.Join(x.tmp, "wt")
	os.Mkdir(d, 0o700)
	os.WriteFile(filepath.Join(d, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	os.WriteFile(filepath.Join(d, "commondir"), []byte(common+"\n"), 0o600)
	return d
}

// git applies url.<base>.insteadOf to the address the check fetches as
// well: a rule that sends it to another workspace makes it not count.
// A remote outside the root can keep its objects in the workspace through
// its own objects/info/alternates: git serves them as the remote's, and
// they go with the workspace. The workspace has a commit only there.
func TestRefuseRemoteBorrowingFromTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason Reason
		remote func(x *removal) string
	}{
		{"absolute", RemoteIsLocal, func(x *removal) string {
			return x.shared(x.clone, "r.git")
		}},
		{"relative", RemoteIsLocal, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			rel, err := filepath.Rel(filepath.Join(r, "objects"), filepath.Join(x.clone, ".git", "objects"))
			if err != nil {
				x.t.Fatal(err)
			}
			x.alternates(r, rel+"\n")
			return r
		}},
		// ".." after a symlink goes up from the link's target. As text
		// this is <tmp>/l/root/..., outside the root.
		{"dot-dot after a symlink", RemoteIsLocal, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			x.alternates(r, x.dotDotLink()+"/../root/"+Dir+"/c/s/k/.git/objects\n")
			return r
		}},
		// remote -> a repository outside the root -> the workspace.
		{"chain", RemoteIsLocal, func(x *removal) string {
			return x.shared(x.shared(x.clone, "m.git"), "r.git")
		}},
		// git reads # lines as comments and unquotes a C-quoted path.
		// \057 is "/": the path leads through <tmp>/l2, a link outside
		// the root, into the workspace's .git.
		{"quoted", RemoteIsLocal, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			l2 := x.symlink(filepath.Join(x.clone, ".git"), filepath.Join(x.tmp, "l2"))
			x.alternates(r, "# a comment\n\n\""+l2+"\\057objects\"\n")
			return r
		}},
		// After the closing quote git skips one byte, whatever it is,
		// and reads the next path from there.
		{"path after a quoted one", RemoteIsLocal, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			x.alternates(r, "\""+filepath.Join(x.bare, "objects")+"\"X"+filepath.Join(x.clone, ".git", "objects")+"\n")
			return r
		}},
		// A quoted path git cannot unquote is read as it is: a relative
		// path, quotes and all, that does not exist.
		{"badly quoted", RemoteUnresolved, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			x.alternates(r, "\""+filepath.Join(x.clone, ".git", "objects")+"\\q\"\n")
			return r
		}},
		// An alternate that does not exist cannot be told: it may be a
		// workspace that is already gone.
		{"missing alternate", RemoteUnresolved, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			x.alternates(r, filepath.Join(x.tmp, "gone", "objects")+"\n")
			return r
		}},
		// The remote's own objects directory is a symlink into the
		// workspace.
		{"objects directory", RemoteIsLocal, func(x *removal) string {
			r := filepath.Join(x.tmp, "r.git")
			x.git(x.tmp, "clone", "-q", "--bare", x.clone, r)
			if err := os.RemoveAll(filepath.Join(r, "objects")); err != nil { // under x.tmp, made just above
				x.t.Fatal(err)
			}
			x.symlink(filepath.Join(x.clone, ".git", "objects"), filepath.Join(r, "objects"))
			return r
		}},
		// git reads alternates six levels deep: the sixth still leads to
		// the workspace.
		{"six levels deep", RemoteIsLocal, func(x *removal) string {
			r := x.clone
			for i := 0; i < 6; i++ {
				r = x.shared(r, "c"+strconv.Itoa(i)+".git")
			}
			return r
		}},
		// A link inside the workspace on the way to objects outside the
		// root breaks when the workspace is moved: an alternate, the
		// objects directory, or the alternates file itself.
		{"alternate through a link in the workspace", RemoteIsLocal, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			x.alternates(r, x.symlink(x.objectsCopy(), filepath.Join(x.clone, ".git", "lnk"))+"\n")
			return r
		}},
		{"objects directory through a link in the workspace", RemoteIsLocal, func(x *removal) string {
			r := filepath.Join(x.tmp, "r.git")
			x.git(x.tmp, "clone", "-q", "--bare", x.clone, r)
			if err := os.RemoveAll(filepath.Join(r, "objects")); err != nil { // under x.tmp, made just above
				x.t.Fatal(err)
			}
			x.symlink(x.symlink(x.objectsCopy(), filepath.Join(x.clone, ".git", "lnk")), filepath.Join(r, "objects"))
			return r
		}},
		{"alternates file through a link in the workspace", RemoteIsLocal, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			file := filepath.Join(x.clone, ".git", "alt")
			os.WriteFile(file, []byte(x.objectsCopy()+"\n"), 0o600)
			alternates := filepath.Join(r, "objects", "info", "alternates")
			os.Remove(alternates) // under x.tmp, written by shared
			x.symlink(file, alternates)
			return r
		}},
		// git takes only a directory as an alternate, and does not go up
		// from a file.
		{"alternate that is a file", RemoteUnresolved, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			f := filepath.Join(x.tmp, "afile")
			os.WriteFile(f, nil, 0o600)
			x.alternates(r, f+"\n")
			return r
		}},
		{"dot-dot after a file", RemoteUnresolved, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			f := filepath.Join(x.tmp, "afile")
			os.WriteFile(f, nil, 0o600)
			x.alternates(r, f+"/../remote/repo.git/objects\n")
			return r
		}},
		// git goes depth first and skips what it has seen: b is reached
		// through a1..a5 at the sixth level, where its alternates are not
		// read, and the direct entry for b is then skipped. What b's
		// alternates hold is never served.
		{"depth first, as git", RemoteUnresolved, func(x *removal) string {
			r := x.shared(x.clone, "r.git")
			b := x.bareRepo("b.git")
			x.alternates(b, filepath.Join(x.bare, "objects")+"\n")
			next := b
			for i := 5; i >= 1; i-- {
				a := x.bareRepo("a" + strconv.Itoa(i) + ".git")
				x.alternates(a, filepath.Join(next, "objects")+"\n")
				next = a
			}
			x.alternates(r, filepath.Join(next, "objects")+"\n"+filepath.Join(b, "objects")+"\n")
			return r
		}},
		// git reads alternates six levels deep and ignores the rest.
		{"deeper than git reads", RemoteUnresolved, func(x *removal) string {
			r := x.clone
			for i := 0; i < 7; i++ {
				r = x.shared(r, "c"+strconv.Itoa(i)+".git")
			}
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRemoval(t)
			x.commit(x.clone, "w") // on no remote but the one below
			x.git(x.clone, "remote", "set-url", "origin", tc.remote(x))
			wantRefusal(t, x.check(), tc.reason, "")
		})
	}
}

// Alternates outside the root, a comment naming the workspace, and a loop
// between two of the remote's alternates: the remote counts.
func TestRemoteWithAlternatesOutsideTheRootCounts(t *testing.T) {
	x := newRemoval(t)
	r := x.shared(x.bare, "r.git")
	a := x.bareRepo("a.git")
	b := x.bareRepo("b.git")
	x.alternates(r, filepath.Join(a, "objects")+"\n# "+filepath.Join(x.clone, ".git", "objects")+"\n")
	x.alternates(a, filepath.Join(x.bare, "objects")+"\n"+filepath.Join(b, "objects")+"\n")
	x.alternates(b, filepath.Join(a, "objects")+"\n")
	x.git(x.clone, "remote", "set-url", "origin", r)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

// git follows alternates six levels deep: a chain that long to a
// repository outside the root counts.
func TestRemoteWithSixLevelsOfAlternatesCounts(t *testing.T) {
	x := newRemoval(t)
	r := x.bare
	for i := 0; i < 6; i++ {
		r = x.shared(r, "c"+strconv.Itoa(i)+".git")
	}
	x.git(x.clone, "remote", "set-url", "origin", r)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

// shared is a bare repository at <tmp>/<name>, outside the root, whose
// main names src's HEAD and whose alternates name only src's objects. It
// is made by hand, not by git clone --shared: that would also copy src's
// own alternates, and newer git refuses to clone from a chain deeper than
// it reads.
func (x *removal) shared(src, name string) string {
	x.t.Helper()
	dst := x.bareRepo(name)
	objects := filepath.Join(src, ".git", "objects")
	if _, err := os.Stat(objects); err != nil {
		objects = filepath.Join(src, "objects")
	}
	x.alternates(dst, objects+"\n")
	head := ""
	if b, err := os.ReadFile(filepath.Join(src, "refs", "heads", "main")); err == nil {
		head = strings.TrimSpace(string(b)) // one made here
	} else {
		head = strings.TrimSpace(x.git(src, "rev-parse", "HEAD"))
	}
	if err := os.WriteFile(filepath.Join(dst, "refs", "heads", "main"), []byte(head+"\n"), 0o600); err != nil {
		x.t.Fatal(err)
	}
	return dst
}

// bareRepo is an empty bare repository at <tmp>/<name>.
func (x *removal) bareRepo(name string) string {
	x.t.Helper()
	r := filepath.Join(x.tmp, name)
	x.git(x.tmp, "init", "-q", "--bare", "-b", "main", r)
	return r
}

// objectsCopy is a copy of the workspace's objects at <tmp>/objects-copy,
// outside the root.
func (x *removal) objectsCopy() string {
	x.t.Helper()
	dst := filepath.Join(x.tmp, "objects-copy")
	if err := os.CopyFS(dst, os.DirFS(filepath.Join(x.clone, ".git", "objects"))); err != nil {
		x.t.Fatal(err)
	}
	return dst
}

func (x *removal) alternates(repo, content string) {
	x.t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "objects", "info", "alternates"), []byte(content), 0o600); err != nil {
		x.t.Fatal(err)
	}
}

// A remote outside the root can keep its objects in the workspace through
// a link below its objects directory: git reads pack/ and the fan-out
// directories through links. The workspace has a commit only there.
func TestRefuseRemoteWithObjectsLinkedIntoTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason Reason
		remote func(x *removal) string
	}{
		{"pack directory", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			x.repack()
			os.Remove(filepath.Join(r, "objects", "pack")) // empty, made by git init under x.tmp
			x.symlink(filepath.Join(x.clone, ".git", "objects", "pack"), filepath.Join(r, "objects", "pack"))
			return r
		}},
		{"pack file", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			x.repack()
			for _, f := range x.packFiles() {
				x.symlink(f, filepath.Join(r, "objects", "pack", filepath.Base(f)))
			}
			return r
		}},
		{"fan-out directory", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			dir, _ := x.looseHead()
			x.symlink(dir, filepath.Join(r, "objects", filepath.Base(dir)))
			return r
		}},
		{"loose object", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			dir, file := x.looseHead()
			x.symlink(filepath.Join(dir, file), filepath.Join(r, "objects", filepath.Base(dir), file))
			return r
		}},
		// The remote's alternate, outside the root, has the link.
		{"in an alternate", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			alt := x.bareRepo("alt.git")
			x.alternates(r, filepath.Join(alt, "objects")+"\n")
			dir, _ := x.looseHead()
			x.symlink(dir, filepath.Join(alt, "objects", filepath.Base(dir)))
			return r
		}},
		// pack/ is a link outside the root, and a file in what it leads
		// to is a link into the workspace.
		{"through a linked pack directory", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			x.repack()
			outside := filepath.Join(x.tmp, "pack-outside")
			os.Mkdir(outside, 0o700)
			for _, f := range x.packFiles() {
				x.symlink(f, filepath.Join(outside, filepath.Base(f)))
			}
			os.Remove(filepath.Join(r, "objects", "pack"))
			x.symlink(outside, filepath.Join(r, "objects", "pack"))
			return r
		}},
		// Every fan-out directory and pack/ link to one directory outside
		// the root, which has the workspace's pack files: looked at from
		// a fan-out directory first, it is still looked at as pack/.
		{"pack and fan-out directories leading to one place", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			x.repack()
			shared := filepath.Join(x.tmp, "shared")
			for _, f := range x.packFiles() {
				x.symlink(f, filepath.Join(shared, filepath.Base(f)))
			}
			for i := 0; i < 256; i++ {
				x.symlink(shared, filepath.Join(r, "objects", fmt.Sprintf("%02x", i)))
			}
			os.Remove(filepath.Join(r, "objects", "pack"))
			x.symlink(shared, filepath.Join(r, "objects", "pack"))
			return r
		}},
		// A multi-pack-index may name a pack below pack/: a link there
		// does not count either.
		{"below pack", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			x.repack()
			for _, f := range x.packFiles() {
				x.symlink(f, filepath.Join(r, "objects", "pack", "sub", filepath.Base(f)))
			}
			return r
		}},
		// info/commit-graphs is a link outside the root, and a file in
		// what it leads to is a link into the workspace.
		{"through a linked commit-graphs directory", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			graph := filepath.Join(x.clone, ".git", "objects", "info", "graph")
			os.WriteFile(graph, nil, 0o600)
			outside := filepath.Join(x.tmp, "graphs-outside")
			x.symlink(graph, filepath.Join(outside, "graph-x.graph"))
			x.symlink(outside, filepath.Join(r, "objects", "info", "commit-graphs"))
			return r
		}},
		// info/ holds the commit-graph, and its chain in commit-graphs/.
		{"commit-graph chain", RemoteIsLocal, func(x *removal) string {
			r := x.namingRemote("r.git")
			graph := filepath.Join(x.clone, ".git", "objects", "info", "graph")
			os.WriteFile(graph, nil, 0o600)
			x.symlink(graph, filepath.Join(r, "objects", "info", "commit-graphs", "graph-x.graph"))
			return r
		}},
		// A link that cannot be resolved cannot be told.
		{"dangling link", RemoteUnresolved, func(x *removal) string {
			r := x.namingRemote("r.git")
			dir, file := x.looseHead()
			os.Mkdir(filepath.Join(r, "objects", filepath.Base(dir)), 0o700)
			x.symlink(filepath.Join(x.tmp, "gone"), filepath.Join(r, "objects", filepath.Base(dir), file))
			return r
		}},
		{"too many entries", RemoteUnresolved, func(x *removal) string {
			r := x.namingRemote("r.git")
			for i := 0; i < 3; i++ {
				os.WriteFile(filepath.Join(r, "objects", "pack", "f"+strconv.Itoa(i)), nil, 0o600)
			}
			x.r.objectLimit = 2
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRemoval(t)
			x.commit(x.clone, "w") // on no remote but the one below
			x.git(x.clone, "remote", "set-url", "origin", tc.remote(x))
			wantRefusal(t, x.check(), tc.reason, "")
		})
	}
}

// The remote's objects are only in a pack outside the root, which its
// pack/ links to. Links into the workspace where git does not read are
// left alone: an upper-case fan-out directory, a name git does not use
// in objects/, a link in a fan-out directory with a name no loose object
// has, and one in a hex-named directory below a fan-out directory. A link
// loop below pack/ is looked at once. The remote counts.
func TestRemoteWithObjectsLinkedOutsideTheRootCounts(t *testing.T) {
	x := newRemoval(t)
	outside := filepath.Join(x.tmp, "outside.git")
	x.git(x.tmp, "clone", "-q", "--bare", "--no-local", x.bare, outside)
	x.git(outside, "repack", "-q", "-a", "-d")
	r := x.namingRemote("r.git")
	objects := filepath.Join(r, "objects")
	os.Remove(filepath.Join(objects, "pack")) // empty, made by git init under x.tmp
	x.symlink(filepath.Join(outside, "objects", "pack"), filepath.Join(objects, "pack"))
	x.git(r, "cat-file", "-e", "main^{commit}")                       // git reads it through the link
	x.symlink(".", filepath.Join(outside, "objects", "pack", "loop")) // looked at once
	ws := filepath.Join(x.clone, ".git", "objects")
	x.symlink(ws, filepath.Join(objects, "AB"))
	x.symlink(ws, filepath.Join(objects, "unused"))
	x.symlink(ws, filepath.Join(objects, "ab", "sub"))
	x.symlink(ws, filepath.Join(objects, "ab", "cd", "ef"))
	x.git(x.clone, "remote", "set-url", "origin", r)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

// namingRemote is an empty bare repository at <tmp>/<name> whose main names
// the workspace's HEAD: it has the ref but none of the objects.
func (x *removal) namingRemote(name string) string {
	x.t.Helper()
	r := x.bareRepo(name)
	head := strings.TrimSpace(x.git(x.clone, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(r, "refs", "heads", "main"), []byte(head+"\n"), 0o600); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// repack packs the workspace's objects.
func (x *removal) repack() {
	x.t.Helper()
	x.git(x.clone, "repack", "-q", "-a", "-d")
}

// packFiles are the files of the workspace's packs.
func (x *removal) packFiles() []string {
	x.t.Helper()
	files, err := filepath.Glob(filepath.Join(x.clone, ".git", "objects", "pack", "pack-*"))
	if err != nil || len(files) == 0 {
		x.t.Fatalf("no pack files: %v", err)
	}
	return files
}

// looseHead is the fan-out directory and file name of the workspace's HEAD
// commit, a loose object.
func (x *removal) looseHead() (string, string) {
	x.t.Helper()
	head := strings.TrimSpace(x.git(x.clone, "rev-parse", "HEAD"))
	dir := filepath.Join(x.clone, ".git", "objects", head[:2])
	if _, err := os.Stat(filepath.Join(dir, head[2:])); err != nil {
		x.t.Fatal(err)
	}
	return dir, head[2:]
}

func TestRefuseRemoteRewrittenAgain(t *testing.T) {
	x := newRemoval(t)
	b := filepath.Join(x.root, Dir, "c", "s", "other", "b.git")
	x.git(x.tmp, "clone", "-q", "--bare", x.bare, b)
	first, second := filepath.Join(x.tmp, "first.git"), filepath.Join(x.tmp, "second.git")
	// The workspace's git rewrites first to second, once; the fetch would
	// rewrite second, an empty repository outside the root, to b.
	x.git(x.tmp, "init", "-q", "--bare", second)
	global := filepath.Join(x.tmp, "gitconfig")
	x.git(x.tmp, "config", "--file", global, "url."+second+".insteadOf", first)
	x.git(x.tmp, "config", "--file", global, "url."+b+".insteadOf", second)
	x.r.Maker.Environ = func() []string { return append(x.env(), "GIT_CONFIG_GLOBAL="+global) }
	x.git(x.clone, "remote", "set-url", "origin", first)
	wantRefusal(t, x.check(), RemoteUnresolved, "")

	// It is left out like any remote that does not count: another
	// remote with every commit lets the removal through.
	x.git(x.clone, "remote", "add", "backup", x.bare)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

// git reads a relative path from the repository, not from where the check
// fetches. From the workspace, <tmp>/root/workspaces/c/s/k, the five ".."
// lead to <tmp>; from the scratch repository, <root>/state/removal/<name>,
// they lead out of it, where nothing is.
func TestRemoteByRelativePathCounts(t *testing.T) {
	x := newRemoval(t)
	base := filepath.Base(x.tmp)
	x.git(x.tmp, "clone", "-q", "--bare", x.bare, filepath.Join(x.tmp, base, "rel.git"))
	x.git(x.clone, "remote", "set-url", "origin", "../../../../../"+base+"/rel.git")
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}

// dotDotLink is <tmp>/l/link, a symlink to <tmp>/nested.
func (x *removal) dotDotLink() string {
	nested := filepath.Join(x.tmp, "nested")
	os.Mkdir(nested, 0o700)
	return x.symlink(nested, filepath.Join(x.tmp, "l", "link"))
}

func TestRefuseRemoteUnderAHataoriRoot(t *testing.T) {
	x := newRemoval(t)
	mirror := filepath.Join(x.root, "mirrors", "repo.git")
	x.git(x.tmp, "clone", "-q", "--bare", x.bare, mirror)
	x.git(x.clone, "remote", "set-url", "origin", "file://"+mirror)
	wantRefusal(t, x.check(), RemoteInRoot, "")

	// git decodes the URL's path: %72 is "r", and the root is in it.
	x.git(x.clone, "remote", "set-url", "origin", "file://"+strings.Replace(mirror, "/root/mirrors/", "/%72oot/mirrors/", 1))
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

// lsof writes a byte that is not printable in its locale as \xNN, and a
// backslash as two.
func TestRefuseProcessUsingAWorkspaceWhosePathLsofEscapes(t *testing.T) {
	for _, tc := range []struct{ root, locale string }{
		{"ルート", ""}, {"ルート", "C"}, {"ルート", "C.UTF-8"},
		{`back\slash`, "C"}, {`back\slash`, "C.UTF-8"},
		{`\xe3\\`, "C"}, {"ü\xfe", "C.UTF-8"}, {"a  b ", "C"},
	} {
		t.Run(fmt.Sprintf("%q in %q", tc.root, tc.locale), func(t *testing.T) {
			if runtime.GOOS == "darwin" && !utf8.ValidString(tc.root) {
				t.Skip("the file system takes UTF-8 names only")
			}
			t.Setenv("LC_ALL", tc.locale) // as the daemon was started with
			if tc.locale == "" {
				t.Setenv("LANG", "")
				t.Setenv("LC_CTYPE", "")
			}
			x := newRemovalIn(t, tc.root)
			if err := x.check(); err != nil {
				t.Fatalf("clean, pushed, unused: %v", err)
			}
			cmd := exec.Command("sleep", "60")
			cmd.Dir = x.clone
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			stop := func() { cmd.Process.Kill(); cmd.Wait() }
			t.Cleanup(stop)
			wantRefusal(t, x.check(), InUse, "")
			stop()

			f, err := os.Open(filepath.Join(x.clone, "a"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			wantRefusal(t, x.check(), InUse, "")

			if runtime.GOOS != "linux" {
				return
			}
			m, err := syscall.Mmap(int(f.Fd()), 0, 1, syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Munmap(m)
			f.Close()
			wantRefusal(t, x.check(), InUse, "")
		})
	}
}

// The check holds the workspace open itself while it asks lsof. Another
// descriptor of the same process is a use like any other.
func TestRefuseWorkspaceTheDaemonHoldsOpen(t *testing.T) {
	x := newRemoval(t)
	f, err := os.Open(x.clone)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	wantRefusal(t, x.check(), InUse, "")
}

// Nor is another process's descriptor the check's own, whatever its number.
func TestRefuseProcessHoldingTheWorkspaceWithTheSameDescriptor(t *testing.T) {
	x := newRemoval(t)
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	held, err := os.Open(x.clone)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.Dir = "/"
	for fd := 3; fd < int(held.Fd()); fd++ {
		cmd.ExtraFiles = append(cmd.ExtraFiles, null)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, held)
	err = cmd.Start()
	held.Close() // the check's own gets this number
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	wantRefusal(t, x.check(), InUse, "")
}

func TestRefuseWorkspaceWhosePathLsofCannotBeComparedWith(t *testing.T) {
	for _, root := range []string{"t\tab", "new\nline", "c\x01", "d\x7f"} {
		if name, ok := lsofName("/" + root + "/workspaces/7"); ok {
			t.Errorf("%q: %q", root, name)
		}
	}
	x := newRemovalIn(t, "t\tab")
	wantRefusal(t, x.check(), UnknownUse, "")
}

// An lsof that answers, but not what is asked, as BusyBox's does, or that
// does not see this process, says nothing about the others.
func TestRefuseWhenLsofDoesNotReportTheWorkspaceHeldOpen(t *testing.T) {
	for name, output := range map[string]string{
		"nothing":       ``,
		"busybox":       `1\t/bin/busybox\t0\t/dev/null\n`,
		"other process": `p1\0\nfcwd\0n/\0\n`,
	} {
		t.Run(name, func(t *testing.T) {
			x := newRemoval(t)
			bin := filepath.Join(x.tmp, "bin")
			os.Mkdir(bin, 0o700)
			if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte("#!/bin/sh\nprintf '"+output+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(filepath.ListSeparator)+os.Getenv("PATH"))
			wantRefusal(t, x.check(), UnknownUse, "")
		})
	}
}

func TestRefuseGitError(t *testing.T) {
	x := newRemoval(t)
	// A directory that is not a repository: git fails to fetch from it.
	// Nothing at all there is RemoteUnresolved.
	notRepo := filepath.Join(x.tmp, "not-a-repository")
	os.Mkdir(notRepo, 0o700)
	x.git(x.clone, "remote", "set-url", "origin", notRepo)
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
	for _, name := range []string{old + "-00000000-a", recent + "-00000000-b", "not-from-removal", "2026-c", "XXXXXXXXXXXXXXXX-d", old + "-backup", "20261399T000000Z-00000000-e", "pending-" + old + "-00000000-f"} {
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
	if want := []string{"2026-c", old + "-backup", recent + "-00000000-b", "20261399T000000Z-00000000-e", "XXXXXXXXXXXXXXXX-d", "not-from-removal", "pending-" + old + "-00000000-f"}; fmt.Sprint(left) != fmt.Sprint(want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	if _, err := os.Stat(filepath.Join(tmp, "outside")); err != nil {
		t.Fatal(err)
	}
}

// The purge takes a name only as Remove writes it: what Segment gives for
// every base name a directory can have, short, escaped, dots only or too
// long and hashed, and nothing else.
func TestTrashNameIsWhatRemoveWrites(t *testing.T) {
	prefix := "20000101T000000Z-0123abcd-"
	segments := []string{"...", "k", "a b", "%", "%zz", "ü", strings.Repeat("x", 200), strings.Repeat("é", 50), strings.Repeat("x", 255), strings.Repeat(".", 255)}
	for n := 30; n <= 90; n++ { // the cut at 40 bytes falls at each place of an escape
		segments = append(segments, strings.Repeat("a", n%3)+strings.Repeat(" ", n), strings.Repeat(".", n))
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	const bytes = "aZ09_-.% \x01\xff\xc3\xa9"
	for range 5000 {
		b := make([]byte, 1+rnd.IntN(120))
		for i := range b {
			b[i] = bytes[rnd.IntN(len(bytes))]
		}
		if s := string(b); s != "." && s != ".." {
			segments = append(segments, s)
		}
	}
	for _, seg := range segments {
		if name := prefix + Segment(seg); !trashName(name) {
			t.Errorf("%q, the name Remove gives for %q, is not taken", name, seg)
		}
	}
	hash := "%abcdef0123456789abcdef01" // "%ab" is no escape Segment writes
	for _, tail := range []string{"%zz", "a%", "%2", "%2e", "%%", "a%g0", "x%ab", "%a", "a/b", "",
		"%41", "a%2D", ".", "..", "a%2E", // escapes Segment does not write, dots it escapes
		"abc" + hash, strings.Repeat("a", 39) + hash, strings.Repeat("a", 41) + hash, // a cut that is not at 40
		strings.Repeat("a", 37) + "%41" + hash, strings.Repeat("a", 38) + "%g" + hash, "%2E.%2E" + strings.Repeat("a", 33) + hash,
		strings.Repeat("a", 40) + "%abcdef0123456789ABCDEF01",
		strings.Repeat("%2E", 13) + "%" + strings.Repeat("0", 24), // the hash of no input of dots
		// What Segment gives for no base name: empty, ".", "..", NUL, '/'.
		Segment(""), Segment("."), Segment(".."), Segment("a\x00"), Segment("a/b"),
		strings.Repeat("a", 37) + "%00" + hash, strings.Repeat("a", 37) + "%2F" + hash} {
		if trashName(prefix + tail) {
			t.Errorf("%q, which Remove does not write, is taken", prefix+tail)
		}
	}
}

// A name has its time in whole seconds, rounded down, so an entry is kept
// for a second longer than the retention from that time.
func TestPurgeKeepsAnEntryWithinTheSecondOfItsName(t *testing.T) {
	tmp := t.TempDir()
	name := "20000101T000000Z-00000000-a"
	os.MkdirAll(filepath.Join(tmp, TrashDir, name), 0o700)
	at := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Remover{Maker: Maker{Root: tmp}, Retention: time.Hour, Now: func() time.Time { return at.Add(time.Hour + 500*time.Millisecond) }}
	if purged, err := r.Purge(); err != nil || len(purged) != 0 {
		t.Fatalf("purged %v, %v within the second of the name", purged, err)
	}
	r.Now = func() time.Time { return at.Add(time.Hour + 1001*time.Millisecond) }
	if purged, err := r.Purge(); err != nil || len(purged) != 1 {
		t.Fatalf("purged %v, %v after it", purged, err)
	}
}

// An entry the purge cannot delete is named in the error, and the old
// entries listed after it are still deleted. Two cannot be deleted, so
// that the error names both whatever order the directory lists them in.
func TestPurgeGoesOnPastAnEntryItCannotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not keep root from deleting in it")
	}
	tmp := t.TempDir()
	trash := filepath.Join(tmp, TrashDir)
	for _, name := range []string{"a", "b", "c", "d"} {
		p := filepath.Join(trash, "20000101T000000Z-00000000-"+name, "d")
		os.MkdirAll(p, 0o700)
		os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o600)
	}
	var stuck []string
	for _, name := range []string{"b", "d"} {
		p := filepath.Join(trash, "20000101T000000Z-00000000-"+name, "d")
		if err := os.Chmod(p, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o700) })
		stuck = append(stuck, p)
	}
	r := Remover{Maker: Maker{Root: tmp}, Retention: time.Hour}
	purged, err := r.Purge()
	if err == nil || !strings.Contains(err.Error(), "20000101T000000Z-00000000-b") || !strings.Contains(err.Error(), "20000101T000000Z-00000000-d") {
		t.Fatalf("purge with two entries it cannot delete: %v", err)
	}
	var names []string
	for _, p := range purged {
		names = append(names, filepath.Base(p))
	}
	sort.Strings(names)
	if fmt.Sprint(names) != "[20000101T000000Z-00000000-a 20000101T000000Z-00000000-c]" {
		t.Fatalf("purged %v", names)
	}
	for _, p := range stuck {
		if _, err := os.Lstat(filepath.Join(p, "f")); err != nil {
			t.Fatal(err)
		}
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
