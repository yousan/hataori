package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRefuseLinkedGitDirectoryDependingOnWorkspace(t *testing.T) {
	for _, common := range []bool{true, false} {
		name := "objects link"
		if common {
			name = "common directory"
		}
		t.Run(name, func(t *testing.T) {
			x := newRemoval(t)
			store := filepath.Join(x.clone, "store")
			external := filepath.Join(x.tmp, "external.git")
			x.git(x.tmp, "clone", "-q", "--bare", x.bare, external)
			if common {
				if err := os.Rename(external, store); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(external, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(store, "HEAD"), filepath.Join(external, "HEAD")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(external, "commondir"), []byte(store+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(store, 0o700); err != nil {
					t.Fatal(err)
				}
				objects := filepath.Join(store, "objects")
				if err := os.Rename(filepath.Join(external, "objects"), objects); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(objects, filepath.Join(external, "objects")); err != nil {
					t.Fatal(err)
				}
			}
			git := func(args ...string) string {
				return x.git(x.tmp, append([]string{"--git-dir", external}, args...)...)
			}
			id := git("commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "workspace-dependent work")
			git("update-ref", "refs/heads/wip", id)
			link := filepath.Join(x.clone, "l.git")
			if err := os.Symlink(external, link); err != nil {
				t.Fatal(err)
			}
			if got := x.git(x.tmp, "--git-dir", link, "rev-parse", "wip"); got != id {
				t.Fatal(got)
			}
			x.write(".git/info/exclude", "/store/\n/l.git\n")
			x.r.Disposable = []string{"store", "l.git"}
			if common {
				wantRefusal(t, x.check(), Worktree, "l.git")
			} else {
				wantRefusal(t, x.check(), Unreadable, "")
			}
		})
	}
}

func TestRemovableWithOrdinaryDirectoryLink(t *testing.T) {
	x := newRemoval(t)
	target := filepath.Join(x.tmp, "ordinary")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(x.clone, "cache")); err != nil {
		t.Fatal(err)
	}
	x.write(".git/info/exclude", "/cache\n")
	x.r.Disposable = []string{"cache"}
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}
