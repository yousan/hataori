package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefuseWorkspaceTopThatIsAlsoAGitDirectory(t *testing.T) {
	x := newRemoval(t)
	// This second repository has no work tree and is beside .git, rather
	// than below the workspace top. Its unique commit is never pushed.
	x.git(x.tmp, "init", "-q", "--bare", x.clone)
	topGit := func(args ...string) string {
		return x.git(x.tmp, append([]string{"--git-dir", x.clone}, args...)...)
	}
	topGit("fetch", "-q", x.bare, "main:refs/heads/base")
	id := topGit("commit-tree", "base^{tree}", "-p", "base", "-m", "only at the workspace top")
	topGit("update-ref", "refs/heads/wip", id)
	if got := topGit("rev-parse", "wip"); got != id {
		t.Fatal(got)
	}
	entries, err := os.ReadDir(x.clone)
	if err != nil {
		t.Fatal(err)
	}
	var ignored []string
	for _, entry := range entries {
		switch entry.Name() {
		case ".git", ".gitignore", "a":
			continue
		}
		x.r.Disposable = append(x.r.Disposable, entry.Name())
		ignored = append(ignored, "/"+entry.Name())
	}
	x.write(".gitignore", strings.Join(ignored, "\n")+"\n")
	x.git(x.clone, "add", ".gitignore")
	x.git(x.clone, "commit", "-q", "-m", "ignore the second repository")
	x.git(x.clone, "push", "-q")
	// With a born HEAD, Git can reject the top-level HEAD file as an
	// ambiguous path before the missing repository discovery matters.
	// An unborn checkout still has its pushed main branch, with an empty
	// index and these explicitly disposable, ignored files.
	x.git(x.clone, "symbolic-ref", "HEAD", "refs/heads/unborn")
	x.git(x.clone, "read-tree", "--empty")
	x.write(".git/info/exclude", "/a\n/.gitignore\n")
	x.r.Disposable = append(x.r.Disposable, "a", ".gitignore")
	if got := x.git(x.clone, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Fatalf("workspace is not clean: %s", got)
	}
	wantRefusal(t, x.check(), Unreadable, "")
	if _, err := os.Stat(filepath.Join(x.clone, "objects")); err != nil {
		t.Fatal("the second object store changed:", err)
	}
}
