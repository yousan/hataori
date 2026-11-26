package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRefuseReflogsBehindLinks(t *testing.T) {
	for _, part := range []string{"", "refs", "refs/heads", "refs/heads/main"} {
		name := part
		if name == "" {
			name = "root"
		}
		for _, dangling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dangling=%v", name, dangling), func(t *testing.T) {
				x := newRemoval(t)
				onlyInAReflog(x, true)
				if err := os.Remove(filepath.Join(x.clone, ".git/logs/HEAD")); err != nil {
					t.Fatal(err)
				}
				wantRefusal(t, x.check(), ReflogOnly, "")
				path := filepath.Join(x.clone, ".git/logs", part)
				away := filepath.Join(x.tmp, "saved-logs")
				if err := os.Rename(path, away); err != nil {
					t.Fatal(err)
				}
				if dangling {
					away = filepath.Join(x.tmp, "missing")
				}
				if err := os.Symlink(away, path); err != nil {
					t.Fatal(err)
				}
				wantRefusal(t, x.check(), Unreadable, "")
			})
		}
	}
}

func TestRefuseReflogNamesGitMayIgnore(t *testing.T) {
	for _, name := range []string{"y.lock", "y~", "y..z", ".hidden"} {
		t.Run(name, func(t *testing.T) {
			x := newRemoval(t)
			onlyInAReflog(x, true)
			if err := os.Remove(filepath.Join(x.clone, ".git/logs/HEAD")); err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, x.check(), ReflogOnly, "")
			heads := filepath.Join(x.clone, ".git/logs/refs/heads")
			if err := os.Rename(filepath.Join(heads, "main"), filepath.Join(heads, name)); err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, x.check(), Unreadable, "")
		})
	}
}

func TestRemovableWithDotAtEndOfRefDirectory(t *testing.T) {
	x := newRemoval(t)
	x.git(x.clone, "checkout", "-q", "-b", "topic./branch")
	x.git(x.clone, "push", "-q", "origin", "HEAD")
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
}
