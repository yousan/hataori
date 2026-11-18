package workspace

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// repoFixture is a bare repository and a clone of it, made with git and a
// configuration of their own.
type repoFixture struct {
	t     *testing.T
	home  string
	bare  string
	clone string
}

func (f *repoFixture) env() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
}

func (f *repoFixture) git(cwd string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = f.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *repoFixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.clone, name), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// newRepo makes a repository whose main branch has one commit with a file
// "a" of three lines, and a clone of it.
func newRepo(t *testing.T) *repoFixture {
	t.Helper()
	dir := t.TempDir()
	f := &repoFixture{t: t, home: dir, bare: filepath.Join(dir, "repo.git"), clone: filepath.Join(dir, "clone")}
	work := filepath.Join(dir, "work")
	os.Mkdir(work, 0o700)
	f.git(work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a"), []byte("one\ntwo\nthree\n"), 0o600)
	f.git(work, "add", "a")
	f.git(work, "commit", "-q", "-m", "first")
	f.git(dir, "clone", "-q", "--bare", work, f.bare)
	f.git(dir, "clone", "-q", f.bare, f.clone)
	return f
}

func (f *repoFixture) state() (control.WorkspaceState, error) {
	return Maker{Environ: f.env}.State(context.Background(), f.clone)
}

func (f *repoFixture) mustState() control.WorkspaceState {
	f.t.Helper()
	st, err := f.state()
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// check compares the numbers of st with want; Fetched only for being set.
func check(t *testing.T, st, want control.WorkspaceState) {
	t.Helper()
	if st.Fetched == nil {
		t.Error("Fetched is not set")
	}
	st.Fetched, want.Fetched = nil, nil
	if st != want {
		t.Errorf("got  %+v\nwant %+v", st, want)
	}
}

func TestStateOfACleanClone(t *testing.T) {
	f := newRepo(t)
	head := f.git(f.clone, "rev-parse", "HEAD")
	check(t, f.mustState(), control.WorkspaceState{Branch: "main", Base: head})
}

func TestStateWithAModifiedFile(t *testing.T) {
	f := newRepo(t)
	head := f.git(f.clone, "rev-parse", "HEAD")
	f.write("a", "one\nTWO\nthree\n")
	check(t, f.mustState(), control.WorkspaceState{Branch: "main", Base: head, Added: 1, Removed: 1, Uncommitted: 1})
}

func TestStateWithAStagedFile(t *testing.T) {
	f := newRepo(t)
	head := f.git(f.clone, "rev-parse", "HEAD")
	f.write("b", "x\ny\n")
	f.git(f.clone, "add", "b")
	check(t, f.mustState(), control.WorkspaceState{Branch: "main", Base: head, Added: 2, Uncommitted: 1})
}

func TestStateWithAnUntrackedFile(t *testing.T) {
	f := newRepo(t)
	head := f.git(f.clone, "rev-parse", "HEAD")
	os.Mkdir(filepath.Join(f.clone, "d"), 0o700)
	f.write("d/c", "x\n")
	check(t, f.mustState(), control.WorkspaceState{Branch: "main", Base: head, Uncommitted: 1})
}

func TestStateWithACommitNotPushed(t *testing.T) {
	f := newRepo(t)
	head := f.git(f.clone, "rev-parse", "HEAD")
	f.write("a", "one\ntwo\nthree\nfour\n")
	f.git(f.clone, "commit", "-q", "-am", "four")
	check(t, f.mustState(), control.WorkspaceState{Branch: "main", Base: head, Added: 1, Unpushed: 1})
}

// The lines are counted against where the branch started, not against
// where the remote's default branch is now.
func TestStateOfABranchAgainstItsBranchPoint(t *testing.T) {
	f := newRepo(t)
	point := f.git(f.clone, "rev-parse", "HEAD")
	f.git(f.clone, "checkout", "-q", "-b", "feature")
	f.write("a", "one\nthree\n")
	f.write("b", "1\n2\n3\n")
	f.git(f.clone, "add", "a", "b")
	f.git(f.clone, "commit", "-q", "-m", "feature")

	// Main moves on elsewhere, and the clone fetches it.
	other := filepath.Join(f.home, "other")
	f.git(f.home, "clone", "-q", f.bare, other)
	os.WriteFile(filepath.Join(other, "z"), []byte("1\n2\n3\n4\n5\n"), 0o600)
	f.git(other, "add", "z")
	f.git(other, "commit", "-q", "-m", "elsewhere")
	f.git(other, "push", "-q", "origin", "main")
	f.git(f.clone, "fetch", "-q")
	if f.git(f.clone, "rev-parse", "origin/main") == point {
		t.Fatal("origin/main did not move")
	}

	check(t, f.mustState(), control.WorkspaceState{Branch: "feature", Base: point, Added: 3, Removed: 1, Unpushed: 1})
}

// snapshot is every file and directory under dir: its mode, its time and
// its bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		v := fmt.Sprintf("%v %d", fi.Mode(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += fmt.Sprintf(" %x", sha256.Sum256(b))
		} else if fi.Mode()&fs.ModeSymlink != 0 {
			l, _ := os.Readlink(p)
			v += " -> " + l
		}
		out[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The check writes nothing: not the files, the index, the refs or the
// reflog, even when git would refresh a stale index.
func TestStateChangesNothing(t *testing.T) {
	f := newRepo(t)
	f.write("a", "one\nTWO\nthree\n")
	f.write("b", "staged\n")
	f.git(f.clone, "add", "b")
	f.write("c", "untracked\n")
	f.write("d", "committed\n")
	f.git(f.clone, "add", "d")
	f.git(f.clone, "commit", "-q", "-m", "d")
	f.git(f.clone, "stash", "-q", "-k")
	f.git(f.clone, "stash", "pop", "-q")
	// A tracked file whose time changed but not its bytes: git status would
	// write the index to record its new time.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(f.clone, "d"), old, old); err != nil {
		t.Fatal(err)
	}

	before := snapshot(t, f.clone)
	time.Sleep(10 * time.Millisecond) // a write in the check would get a new time
	for range 2 {
		if _, err := f.state(); err != nil {
			t.Fatal(err)
		}
	}
	after := snapshot(t, f.clone)
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed:\n before %s\n after  %s", p, v, after[p])
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s appeared", p)
		}
	}
}

func TestStateOfADirectoryThatIsNoRepository(t *testing.T) {
	f := newRepo(t)
	for name, dir := range map[string]string{
		"not a repository": t.TempDir(),
		"inside a clone":   filepath.Join(f.clone, "sub"),
		"missing":          filepath.Join(f.home, "missing"),
	} {
		os.MkdirAll(filepath.Join(f.clone, "sub"), 0o700)
		st, err := Maker{Environ: f.env}.State(context.Background(), dir)
		if err == nil {
			t.Errorf("%s: no error, %+v", name, st)
		}
		if st != (control.WorkspaceState{}) {
			t.Errorf("%s: numbers with the error: %+v", name, st)
		}
	}
}

func TestStateWhenGitFails(t *testing.T) {
	f := newRepo(t)
	// main names a commit that does not exist.
	if err := os.WriteFile(filepath.Join(f.clone, ".git", "refs", "heads", "main"), []byte(strings.Repeat("1", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := f.state()
	if err == nil {
		t.Fatalf("no error, %+v", st)
	}
	if st != (control.WorkspaceState{}) {
		t.Errorf("numbers with the error: %+v", st)
	}
}

// A filter driver's clean command could write anything; the check runs none.
func TestStateRunsNoFilter(t *testing.T) {
	f := newRepo(t)
	record := filepath.Join(f.home, "filter-ran")
	f.git(f.clone, "config", "filter.rec.clean", "sh -c 'echo ran >> "+record+"; cat'")
	f.git(f.clone, "config", "filter.rec.process", "sh -c 'echo ran >> "+record+"; exit 1'")
	f.write(".gitattributes", "a filter=rec\n")
	f.write("a", "one\nTWO\nthree\n")
	os.Remove(record)
	st := f.mustState()
	if _, err := os.Stat(record); err == nil {
		t.Fatal("a filter ran")
	}
	if st.Added != 1 || st.Removed != 1 || st.Uncommitted != 2 {
		t.Errorf("%+v", st)
	}
}

// A partial clone would fetch the objects it lacks from the remote.
func TestStateRefusesAPartialClone(t *testing.T) {
	f := newRepo(t)
	f.git(f.clone, "config", "remote.origin.promisor", "true")
	if st, err := f.state(); err == nil || st != (control.WorkspaceState{}) {
		t.Fatalf("%+v, %v", st, err)
	}
}

// A branch whose upstream is configured but gone has no known base.
func TestStateWithAGoneUpstream(t *testing.T) {
	f := newRepo(t)
	f.git(f.clone, "remote", "set-head", "origin", "--delete")
	f.git(f.clone, "checkout", "-q", "-b", "feature")
	f.git(f.clone, "config", "branch.feature.remote", "origin")
	f.git(f.clone, "config", "branch.feature.merge", "refs/heads/feature")
	if st, err := f.state(); err == nil || st != (control.WorkspaceState{}) {
		t.Fatalf("%+v, %v", st, err)
	}
	// With no upstream at all, the base is HEAD.
	f.git(f.clone, "config", "--unset", "branch.feature.merge")
	f.git(f.clone, "config", "--unset", "branch.feature.remote")
	// Deleting origin/HEAD removed the only remote-tracking reflog, so
	// Fetched is not known.
	head := f.git(f.clone, "rev-parse", "HEAD")
	if st := f.mustState(); st != (control.WorkspaceState{Branch: "feature", Base: head}) {
		t.Errorf("%+v", st)
	}
}
