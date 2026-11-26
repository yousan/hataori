package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func gitDirectoryWithTrailingSpace(t *testing.T, gitfile, decoy bool) (*removal, string) {
	t.Helper()
	x := newRemoval(t)
	gitDir := filepath.Join(x.tmp, "gd ")
	path := filepath.Join(x.clone, ".git")
	if err := os.Rename(path, gitDir); err != nil {
		t.Fatal(err)
	}
	if gitfile {
		if err := os.WriteFile(path, []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Symlink(gitDir, path); err != nil {
		t.Fatal(err)
	}
	if decoy {
		x.git(x.tmp, "clone", "-q", "--bare", x.bare, filepath.Join(x.tmp, "gd"))
	}
	return x, gitDir
}

func TestRemovableWithGitDirectoryEndingInSpace(t *testing.T) {
	for _, gitfile := range []bool{false, true} {
		name := "link"
		if gitfile {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			x, dir := gitDirectoryWithTrailingSpace(t, gitfile, false)
			g := reader{dir: x.clone, env: x.env()}
			if got, err := g.line(context.Background(), "rev-parse", "--absolute-git-dir"); err != nil || got != dir {
				t.Fatalf("git directory = %q, %v; want %q", got, err, dir)
			}
			if err := x.check(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefuseHiddenRefWithWhitespacePathAndDecoy(t *testing.T) {
	for _, gitfile := range []bool{false, true} {
		name := "link"
		if gitfile {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			x, dir := gitDirectoryWithTrailingSpace(t, gitfile, true)
			id := x.git(x.clone, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "hidden work")
			// Git's ref enumeration ignores this file. The independent
			// filesystem check must still read it from the actual directory.
			if err := os.WriteFile(filepath.Join(dir, "refs/heads/wip.lock"), []byte(id+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, x.check(), OtherRef, "")
		})
	}
}

func TestReaderLinePreservesSurroundingWhitespace(t *testing.T) {
	x := newRemoval(t)
	want := "  path \t "
	x.git(x.clone, "config", "test.path", want)
	g := reader{dir: x.clone, env: x.env()}
	if got, err := g.line(context.Background(), "config", "--get", "test.path"); err != nil || got != want {
		t.Fatalf("line = %q, %v; want %q", got, err, want)
	}
}
