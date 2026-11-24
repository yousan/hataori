package workspace

import (
	"bytes"
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
	// git's automatic maintenance is off: a commit starts it in the
	// background, and its files come and go while a test reads the
	// directory.
	cmd := exec.Command("git", append([]string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)...)
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

// addSubmodule makes a repository with the file "s" and adds it to the
// repository at dir, as its submodule name.
func (f *repoFixture) addSubmodule(dir, name string) string {
	f.t.Helper()
	work := filepath.Join(f.home, name+"-work")
	os.Mkdir(work, 0o700)
	f.git(work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "s"), []byte("s\n"), 0o600)
	f.git(work, "add", "s")
	f.git(work, "commit", "-q", "-m", "s")
	bare := filepath.Join(f.home, name+".git")
	f.git(f.home, "clone", "-q", "--bare", work, bare)
	f.git(dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", bare, name)
	f.git(dir, "commit", "-q", "-m", "add "+name)
	return filepath.Join(dir, name)
}

// recordFilter configures a filter driver for every file of the repository
// at dir, which makes the file it returns.
func (f *repoFixture) recordFilter(dir, driver, command string) string {
	f.t.Helper()
	ran := filepath.Join(f.home, "filter-ran")
	f.git(dir, "config", "filter."+driver+"."+command, "touch "+ran+"; cat")
	attributes := f.git(dir, "rev-parse", "--path-format=absolute", "--git-path", "info/attributes")
	os.MkdirAll(filepath.Dir(attributes), 0o700)
	if err := os.WriteFile(attributes, []byte("* filter="+driver+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return ran
}

// git runs git status in each submodule, where the submodule's configuration
// names filters of its own.
func TestStateRunsNoFilterOfASubmodule(t *testing.T) {
	for name, add := range map[string]func(f *repoFixture) string{
		"submodule": func(f *repoFixture) string { return f.addSubmodule(f.clone, "sub") },
		"second submodule": func(f *repoFixture) string {
			f.addSubmodule(f.clone, "first")
			return f.addSubmodule(f.clone, "second")
		},
		"submodule of a submodule": func(f *repoFixture) string {
			sub := f.addSubmodule(f.clone, "sub")
			inner := f.addSubmodule(sub, "inner")
			f.git(f.clone, "commit", "-q", "-a", "-m", "move sub")
			return inner
		},
		"gitlink with no .gitmodules": func(f *repoFixture) string {
			dir := filepath.Join(f.clone, "own")
			f.git(f.home, "clone", "-q", f.bare, dir)
			os.Remove(filepath.Join(dir, "a"))
			os.WriteFile(filepath.Join(dir, "s"), []byte("s\n"), 0o600)
			f.git(dir, "add", "-A")
			f.git(dir, "commit", "-q", "-m", "s")
			f.git(f.clone, "-c", "advice.addEmbeddedRepo=false", "add", "own")
			f.git(f.clone, "commit", "-q", "-m", "add own")
			return dir
		},
		"link in the place of a submodule": func(f *repoFixture) string {
			sub := f.addSubmodule(f.clone, "sub")
			other := filepath.Join(f.home, "other")
			f.git(f.home, "clone", "-q", filepath.Join(f.home, "sub.git"), other)
			os.RemoveAll(sub)
			if err := os.Symlink(other, sub); err != nil {
				f.t.Fatal(err)
			}
			return other
		},
		"submodule below a directory with a blank": func(f *repoFixture) string {
			sub := f.addSubmodule(f.clone, "sub")
			os.Mkdir(filepath.Join(f.clone, "a b"), 0o700)
			f.git(f.clone, "mv", "sub", "a b/\u00e9")
			f.git(f.clone, "commit", "-q", "-m", "move")
			return strings.TrimSuffix(sub, "sub") + "a b/\u00e9"
		},
	} {
		for _, command := range []string{"clean", "process"} {
			t.Run(name+", "+command, func(t *testing.T) {
				f := newRepo(t)
				dir := add(f)
				ran := f.recordFilter(dir, "x", command)
				// Of the size the index has, or git does not read it.
				if err := os.WriteFile(filepath.Join(dir, "s"), []byte("t\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				st, err := f.state()
				if _, rerr := os.Lstat(ran); rerr == nil {
					t.Fatal("State ran the filter that the submodule's configuration names")
				}
				if err != nil || st.Uncommitted != 1 {
					t.Errorf("%+v, %v; want the modified submodule counted", st, err)
				}
			})
		}
	}
}

// git asks for safe.directory in the repository it was started in, not in
// the submodules it goes into.
func TestStateRunsNoFilterOfASubmoduleOfAnotherOwner(t *testing.T) {
	f := newRepo(t)
	sub := f.addSubmodule(f.clone, "sub")
	ran := f.recordFilter(sub, "x", "clean")
	if err := os.WriteFile(filepath.Join(sub, "s"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	top, err := filepath.EvalSymlinks(f.clone)
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(f.home, "gitconfig")
	f.git(f.home, "config", "--file", global, "safe.directory", top)
	st, err := Maker{Environ: func() []string {
		return append(f.env(), "GIT_CONFIG_GLOBAL="+global, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
	}}.State(context.Background(), f.clone)
	if _, rerr := os.Lstat(ran); rerr == nil {
		t.Fatal("State ran the filter that the submodule's configuration names")
	}
	if err != nil || st.Uncommitted != 1 {
		t.Errorf("%+v, %v; want the modified submodule counted", st, err)
	}
}

// A submodule that is not checked out has no git status run in it.
func TestStateWithASubmoduleThatIsNotThere(t *testing.T) {
	for name, change := range map[string]func(f *repoFixture, sub string) error{
		"not checked out": func(f *repoFixture, sub string) error {
			f.git(f.clone, "submodule", "deinit", "-q", "sub")
			return nil
		},
		"directory gone": func(f *repoFixture, sub string) error { return os.RemoveAll(sub) },
		"link to itself": func(f *repoFixture, sub string) error {
			os.RemoveAll(sub)
			return os.Symlink("sub", sub)
		},
		"file": func(f *repoFixture, sub string) error {
			os.RemoveAll(sub)
			return os.WriteFile(sub, nil, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRepo(t)
			if err := change(f, f.addSubmodule(f.clone, "sub")); err != nil {
				t.Fatal(err)
			}
			f.mustState()
		})
	}
}

// git status goes round until the path is too long.
func TestDisarmWithASubmoduleThatLinksToItsOwnRepository(t *testing.T) {
	f := newRepo(t)
	sub := f.addSubmodule(f.clone, "sub")
	os.RemoveAll(sub)
	if err := os.Symlink(".", sub); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir, err := filepath.EvalSymlinks(f.clone)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&reader{dir: dir, env: f.env()}).disarm(ctx); err != nil {
		t.Fatal(err)
	}
}

// git's -c takes the first "=" for the end of the key, and would turn off
// another filter.
func TestStateRunsNoFilterWithAnEqualsSignInItsName(t *testing.T) {
	for name, dir := range map[string]func(f *repoFixture) string{
		"workspace": func(f *repoFixture) string { return f.clone },
		"submodule": func(f *repoFixture) string { return f.addSubmodule(f.clone, "sub") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRepo(t)
			dir := dir(f)
			file := filepath.Join(dir, f.git(dir, "ls-files", "a", "s"))
			old, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			ran := f.recordFilter(dir, "x=y", "clean")
			f.git(dir, "config", "filter.x=y.process", "touch "+ran+"; exit 1")
			os.WriteFile(file, bytes.Repeat([]byte("t"), len(old)), 0o600)
			st, err := f.state()
			if _, rerr := os.Lstat(ran); rerr == nil {
				t.Fatal("a filter ran")
			}
			if err != nil || st.Uncommitted != 1 {
				t.Errorf("%+v, %v", st, err)
			}
		})
	}
}

// git fails when it may not run a required filter.
func TestStateCountsAFileUnderARequiredFilterOfASubmodule(t *testing.T) {
	f := newRepo(t)
	sub := f.addSubmodule(f.clone, "sub")
	ran := f.recordFilter(sub, "x", "clean")
	f.git(sub, "config", "filter.x.required", "true")
	os.WriteFile(filepath.Join(sub, "s"), []byte("t\n"), 0o600)
	if st := f.mustState(); st.Uncommitted != 1 {
		t.Errorf("%+v", st)
	}
	if _, err := os.Lstat(ran); err == nil {
		t.Fatal("a filter ran")
	}
}

// The submodules of a submodule are in its work tree, not in its directory.
func TestStateRefusesASubmoduleWithItsWorkTreeElsewhere(t *testing.T) {
	for name, set := range map[string]func(f *repoFixture) (sub, top string){
		"another directory": func(f *repoFixture) (string, string) {
			return f.addSubmodule(f.clone, "sub"), filepath.Join(f.home, "elsewhere")
		},
		"the workspace": func(f *repoFixture) (string, string) {
			return f.addSubmodule(f.clone, "sub"), f.clone
		},
		"submodule of a submodule": func(f *repoFixture) (string, string) {
			return f.addSubmodule(f.addSubmodule(f.clone, "outer"), "sub"), filepath.Join(f.home, "elsewhere")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRepo(t)
			sub, top := set(f)
			f.addSubmodule(sub, "inner")
			f.git(f.home, "clone", "-q", filepath.Join(f.home, "inner.git"), filepath.Join(top, "inner"))
			ran := f.recordFilter(filepath.Join(top, "inner"), "x", "clean")
			os.WriteFile(filepath.Join(top, "inner", "s"), []byte("t\n"), 0o600)
			f.git(sub, "config", "core.worktree", top)
			st, err := f.state()
			if _, rerr := os.Lstat(ran); rerr == nil {
				t.Fatal("a filter ran")
			}
			if err == nil || st != (control.WorkspaceState{}) {
				t.Fatalf("%+v, %v", st, err)
			}
		})
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
