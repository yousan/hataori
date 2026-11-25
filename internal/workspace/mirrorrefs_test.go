package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// mirrorFixture is an upstream repository with a working copy to push from,
// and a Maker with a mirror of it.
type mirrorFixture struct {
	*removal
	m      Maker
	work   string
	mirror string
	n      int
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	x := &mirrorFixture{removal: newRemoval(t)}
	x.m = x.r.Maker
	x.work = filepath.Join(x.tmp, "pushes")
	x.git(x.tmp, "clone", "-q", x.bare, x.work)
	x.mirror = filepath.Join(x.root, "mirrors", MirrorKey(x.bare)+".git")
	return x
}

// make makes a workspace, and by that the mirror's update.
func (x *mirrorFixture) make() string {
	x.t.Helper()
	x.n++
	dir, err := x.m.Clone(context.Background(), control.ThreadID(x.n), "file://"+x.bare, nil)
	if err != nil {
		x.t.Fatal(err)
	}
	return dir
}

// push pushes a new commit on top of parent to each ref, by force, and
// returns it.
func (x *mirrorFixture) push(parent string, refs ...string) string {
	x.t.Helper()
	x.n++
	id := x.git(x.work, "commit-tree", "-p", parent, "-m", fmt.Sprint("c", x.n), "main^{tree}")
	for _, ref := range refs {
		x.git(x.work, "push", "-q", "-f", "origin", id+":"+ref)
	}
	return id
}

// pushTag pushes an annotated tag of id, and returns the tag object.
func (x *mirrorFixture) pushTag(name, id string) string {
	x.t.Helper()
	x.git(x.work, "tag", "-f", "-a", "-m", name, name, id)
	x.git(x.work, "push", "-q", "-f", "origin", "refs/tags/"+name)
	return x.git(x.work, "rev-parse", "refs/tags/"+name)
}

func (x *mirrorFixture) deleteUpstream(refs ...string) {
	x.t.Helper()
	for _, ref := range refs {
		x.git(x.bare, "update-ref", "-d", ref)
	}
	x.git(x.bare, "gc", "-q", "--prune=now")
}

// refs are the refs of the mirror below prefix, each as "<id> <name>".
func (x *mirrorFixture) refs(prefix string) []string {
	x.t.Helper()
	out := x.git(x.mirror, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func (x *mirrorFixture) wantKept(ids ...string) {
	x.t.Helper()
	var want []string
	for _, id := range ids {
		want = append(want, id+" "+keptRefs+id)
	}
	got := x.refs(keptRefs)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		x.t.Fatalf("kept refs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// wantSame fails when the mirror's branches and tags are not upstream's, or
// something fetched is left beside them.
func (x *mirrorFixture) wantSame() {
	x.t.Helper()
	f := []string{"for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags"}
	if got, want := x.git(x.mirror, f...), x.git(x.bare, f...); got != want {
		x.t.Fatalf("the mirror:\n%s\nupstream:\n%s", got, want)
	}
	if got := x.refs(fetchedRefs); got != nil {
		x.t.Fatalf("left: %v", got)
	}
}

// wantHeld fails when one of the objects goes by a gc of the mirror.
func (x *mirrorFixture) wantHeld(ids ...string) {
	x.t.Helper()
	dir, err := os.MkdirTemp(x.tmp, "gc-")
	if err != nil {
		x.t.Fatal(err)
	}
	copied := filepath.Join(dir, "m.git")
	x.copy(x.mirror, copied)
	x.git(copied, "gc", "-q", "--prune=now")
	for _, id := range ids {
		if kind := x.git(copied, "cat-file", "-t", id); kind != "commit" && kind != "tag" {
			x.t.Fatalf("%s is a %s", id, kind)
		}
	}
}

func (x *mirrorFixture) copy(from, to string) {
	x.t.Helper()
	if out, err := exec.Command("cp", "-R", from, to).CombinedOutput(); err != nil {
		x.t.Fatalf("%v: %s", err, out)
	}
}

// wrapGit puts a git before the real one that runs script first, where "$n"
// counts the calls on the mirror and $real is git. It returns how to take it away.
func (x *mirrorFixture) wrapGit(script string) (restore func()) {
	x.t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		x.t.Fatal(err)
	}
	bin, err := os.MkdirTemp(x.tmp, "bin-")
	if err != nil {
		x.t.Fatal(err)
	}
	body := "#!/bin/sh\nreal='" + real + "'\nbin='" + bin + "'\ncase \" $* \" in *' --git-dir " + x.mirror + " '*)\necho x >> \"$bin/count\"\nn=$(wc -l < \"$bin/count\")\n" + script + "\n;; esac\nexec \"$real\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0o700); err != nil {
		x.t.Fatal(err)
	}
	old := os.Getenv("PATH")
	os.Setenv("PATH", bin+string(os.PathListSeparator)+old)
	restore = func() { os.Setenv("PATH", old) }
	x.t.Cleanup(restore)
	return restore
}

// A workspace made after a branch and an annotated tag were deleted upstream
// has neither, and can be removed; the mirror still holds both.
func TestMirrorFollowsDeletions(t *testing.T) {
	x := newMirrorFixture(t)
	gone := x.push("main", "refs/heads/gone")
	tag := x.pushTag("v1", "main")
	before := x.make()
	x.deleteUpstream("refs/heads/gone", "refs/tags/v1")
	after := x.make()

	if got := x.git(after, "for-each-ref", "--format=%(refname)"); got != "refs/heads/main\nrefs/remotes/origin/HEAD\nrefs/remotes/origin/main" {
		t.Fatalf("refs of the workspace:\n%s", got)
	}
	if err := x.r.Check(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	// One made earlier holds them, and refuses as it did.
	if err := x.r.Check(context.Background(), before); err == nil {
		t.Fatal("a workspace with the only copy can be removed")
	}
	x.wantSame()
	x.wantKept(gone, tag)
	x.wantHeld(gone, tag)
	// And it stays so.
	x.make()
	x.push("main", "refs/heads/main")
	x.make()
	x.wantSame()
	x.wantKept(gone, tag)
}

// Only what the refs left do not reach is kept.
func TestMirrorKeepsOnlyWhatIsNotReached(t *testing.T) {
	x := newMirrorFixture(t)
	main := x.git(x.work, "rev-parse", "main")
	merged := x.push(main, "refs/heads/merged")
	renamed := x.push(main, "refs/heads/old-name")
	lower := x.push(main, "refs/heads/lower")
	upper := x.push(lower, "refs/heads/upper")
	swapA := x.push(main, "refs/heads/swap-a")
	swapB := x.push(main, "refs/heads/swap-b")
	amended := x.push(main, "refs/heads/amended")
	inner := x.pushTag("inner", main)
	x.pushTag("outer", inner)
	plain := x.push(main, "refs/tags/light")
	moved := x.pushTag("moved", main)
	ofGone := x.push(main, "refs/heads/of-gone")
	tagOfGone := x.pushTag("of-gone", ofGone)
	blob := x.git(x.work, "rev-parse", "main:a")
	x.git(x.work, "push", "-q", "origin", blob+":refs/tags/blob", "main^{tree}:refs/tags/tree")
	x.make()

	next := x.push(merged, "refs/heads/main") // a fast-forward that merges "merged"
	x.git(x.bare, "update-ref", "refs/heads/new-name", renamed)
	x.git(x.bare, "update-ref", "refs/heads/swap-a", swapB)
	x.git(x.bare, "update-ref", "refs/heads/swap-b", swapA)
	x.push(main, "refs/heads/amended")
	x.pushTag("moved", next)
	x.deleteUpstream("refs/heads/merged", "refs/heads/old-name", "refs/heads/lower", "refs/heads/upper", "refs/tags/inner",
		"refs/tags/light", "refs/heads/of-gone", "refs/tags/of-gone")
	x.make()
	x.wantSame()
	x.wantKept(upper, amended, plain, moved, tagOfGone)
	x.wantHeld(merged, renamed, lower, upper, swapA, swapB, amended, inner, plain, moved, ofGone, tagOfGone)

	// A tree or blob that only a tag named is kept as well.
	content := filepath.Join(x.tmp, "content")
	os.WriteFile(content, []byte("only in a tag\n"), 0o600)
	lone := x.git(x.work, "hash-object", "-w", content)
	x.git(x.work, "push", "-q", "origin", lone+":refs/tags/lone")
	x.make()
	x.deleteUpstream("refs/tags/lone")
	x.make()
	x.wantSame()
	x.wantKept(upper, amended, plain, moved, tagOfGone, lone)
	// What comes back upstream and goes again is kept already.
	x.git(x.work, "push", "-q", "origin", upper+":refs/heads/again")
	x.make()
	x.deleteUpstream("refs/heads/again")
	x.make()
	x.wantSame()
	x.wantKept(upper, amended, plain, moved, tagOfGone, lone)
}

// drop pushes what the mirror is to have, has the mirror made, and then drops
// it upstream in each way. It returns what the mirror must not lose.
func (x *mirrorFixture) drop() (ids []string) {
	x.t.Helper()
	main := x.git(x.work, "rev-parse", "main")
	ids = append(ids, x.push(main, "refs/heads/gone"), x.push(main, "refs/heads/amended"), x.push(main, "refs/heads/merged"),
		x.push(main, "refs/heads/dir"), x.pushTag("v1", main), x.pushTag("v2", main), x.push(main, "refs/tags/light"))
	x.make()
	x.push(ids[2], "refs/heads/main")
	x.push(main, "refs/heads/amended")
	x.pushTag("v2", ids[2])
	x.deleteUpstream("refs/heads/gone", "refs/heads/merged", "refs/heads/dir", "refs/tags/v1", "refs/tags/light")
	x.push(main, "refs/heads/dir/below")
	return ids
}

// Wherever an update of the mirror ends, all that a ref of the mirror named is
// reachable, and the next update ends as it would have.
func TestMirrorUpdateEndsAnywhere(t *testing.T) {
	x := newMirrorFixture(t)
	ids := x.drop()
	was := filepath.Join(x.tmp, "was.git")
	x.copy(x.mirror, was)
	ended := filepath.Join(x.tmp, "ended")
	// "-ge": it dies, and no git runs from there on. "-eq": one git fails.
	for _, cond := range []string{"-ge", "-eq"} {
		for at := 1; ; at++ {
			os.RemoveAll(x.mirror)
			x.copy(was, x.mirror)
			os.Remove(ended)
			restore := x.wrapGit(fmt.Sprintf("[ $n %s %d ] && { : > '%s'; exit 1; }", cond, at, ended))
			mirrorRepo{dir: x.mirror, env: x.m.gitEnv()}.update(context.Background(), "file://"+x.bare)
			restore()
			x.wantHeld(ids...)
			mirrorRepo{dir: x.mirror, env: x.m.gitEnv()}.update(context.Background(), "file://"+x.bare)
			x.wantSame()
			x.wantHeld(ids...)
			if _, err := os.Stat(ended); err != nil {
				if at < 10 {
					t.Fatalf("an update runs git %d times", at-1)
				}
				break
			}
		}
	}
}

// When nothing can be kept nothing is dropped; what is new still comes.
func TestMirrorDropsNothingItCannotKeep(t *testing.T) {
	for name, block := range map[string]func(x *mirrorFixture, id string){
		"no ref can be made": func(x *mirrorFixture, id string) {
			x.git(x.mirror, "update-ref", strings.TrimSuffix(keptRefs, "/"), "refs/heads/main")
		},
		"another object has its name": func(x *mirrorFixture, id string) {
			x.git(x.mirror, "update-ref", keptRefs+id, "refs/heads/main")
		},
		"a symbolic ref has its name": func(x *mirrorFixture, id string) {
			x.git(x.mirror, "symbolic-ref", keptRefs+id, "refs/heads/gone")
		},
		"the refs cannot be hidden": func(x *mirrorFixture, id string) {
			x.git(x.mirror, "config", "--unset-all", "transfer.hideRefs")
			os.WriteFile(filepath.Join(x.mirror, "config.lock"), nil, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newMirrorFixture(t)
			gone := x.push("main", "refs/heads/gone")
			x.make()
			x.make()
			block(x, gone)
			old := x.refs("refs/heads/gone")
			had := x.refs(keptRefs)
			x.deleteUpstream("refs/heads/gone")
			added := x.push("main", "refs/heads/added")
			x.make()
			if got := x.refs("refs/heads/gone"); !slices.Equal(got, old) {
				t.Fatalf("dropped: %v", got)
			}
			if got := x.refs(keptRefs); !slices.Equal(got, had) {
				t.Fatalf("kept refs changed: %v, they were %v", got, had)
			}
			if got := x.refs("refs/heads/added"); len(got) != 1 || !strings.HasPrefix(got[0], added) {
				t.Fatalf("added: %v", got)
			}
		})
	}
}

// Upstream moves on between what the mirror fetched and what it drops.
func TestMirrorUpstreamMovesMeanwhile(t *testing.T) {
	x := newMirrorFixture(t)
	ids := x.drop()
	x.push("main", "refs/heads/second")
	x.make()
	for call := 1; call <= 12; call++ {
		next := x.push("main")
		restore := x.wrapGit(fmt.Sprintf("[ $n -eq %d ] && { \"$real\" -C '%s' push -q -f origin %s:refs/heads/main %s:refs/heads/amended :refs/heads/second >/dev/null 2>&1; }", call, x.work, next, next))
		before := x.git(x.mirror, "for-each-ref", "--format=%(objectname)")
		mirrorRepo{dir: x.mirror, env: x.m.gitEnv()}.update(context.Background(), "file://"+x.bare)
		restore()
		x.wantHeld(append(strings.Fields(before), ids...)...)
		x.push("main", "refs/heads/second")
		x.make()
		x.wantSame()
	}
}

// Updates at once, from two Makers as of two processes.
func TestMirrorUpdatesAtOnce(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	main := x.git(x.work, "rev-parse", "main")
	var ids []string
	for round := 0; round < 4; round++ {
		a, b := x.push(main, "refs/heads/a"), x.push(main, fmt.Sprint("refs/heads/b", round))
		x.make()
		ids = append(ids, a, b)
		x.deleteUpstream(fmt.Sprint("refs/heads/b", round))
		x.push(main, "refs/heads/a")
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m := x.m
				source, unlock := m.mirror(context.Background(), "file://"+x.bare)
				defer unlock()
				if source != x.mirror {
					t.Errorf("source %s", source)
				}
			}()
		}
		wg.Wait()
		x.wantSame()
		x.wantKept(ids...)
		// The next round's push drops it.
		ids = append(ids, strings.Fields(x.refs("refs/heads/a")[0])[0])
	}
}

// A kept ref is shown to nobody: not to a clone, a fetch or a push, and not as
// what a remote has.
func TestMirrorKeptRefsAreNotShown(t *testing.T) {
	x := newMirrorFixture(t)
	gone := x.push("main", "refs/heads/gone")
	x.make()
	x.deleteUpstream("refs/heads/gone")
	x.make()
	x.wantKept(gone)
	if out := x.git(x.tmp, "ls-remote", x.mirror); strings.Contains(out, "hataori") || strings.Contains(out, gone) {
		t.Fatalf("shown:\n%s", out)
	}
	for _, flag := range []string{"--bare", "--mirror"} {
		dir := filepath.Join(x.tmp, "clone"+flag)
		x.git(x.tmp, "clone", "-q", flag, x.mirror, dir)
		if out := x.git(dir, "for-each-ref"); strings.Contains(out, "hataori") || strings.Contains(out, gone) {
			t.Fatalf("cloned %s:\n%s", flag, out)
		}
	}
	// Whoever's configuration it is, a workspace gets none.
	global := filepath.Join(x.tmp, "global")
	x.git(x.tmp, "config", "--file", global, "remote.origin.fetch", "+refs/*:refs/all/*")
	x.m.Environ = func() []string { return append(x.env(), "GIT_CONFIG_GLOBAL="+global) }
	dir := x.make()
	if out := x.git(dir, "for-each-ref"); strings.Contains(out, "hataori") || strings.Contains(out, gone) {
		t.Fatalf("in the workspace:\n%s", out)
	}
	cmd := exec.Command("git", "-C", x.mirror, "receive-pack", "--advertise-refs", ".")
	cmd.Env = x.env()
	if out, err := cmd.Output(); err != nil || strings.Contains(string(out), "hataori") {
		t.Fatalf("%v: %q", err, out)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("alternates: %v", err)
	}
	// What only the mirror holds is on no remote: a workspace that names it
	// refuses, the mirror as its remote or not.
	x.m.Environ = x.env
	dir = x.make()
	x.git(dir, "update-ref", "refs/heads/again", gone)
	for i := 0; i < 2; i++ {
		if err := x.r.Check(context.Background(), dir); err == nil {
			t.Fatal("a kept ref counts as a remote's")
		}
		if i == 0 {
			x.git(dir, "remote", "add", "mirror", x.mirror)
		}
	}
}

// Upstream deletes the branch a and makes a/b, and the other way round.
func TestMirrorBranchBecomesDirectory(t *testing.T) {
	x := newMirrorFixture(t)
	a := x.push("main", "refs/heads/a")
	x.make()
	x.deleteUpstream("refs/heads/a")
	below := x.push("main", "refs/heads/a/b")
	x.make()
	x.wantSame()
	x.wantKept(a)
	x.deleteUpstream("refs/heads/a/b")
	x.push("main", "refs/heads/a")
	x.make()
	x.wantSame()
	x.wantKept(a, below)
}

// A mirror from before hataori pruned: with what upstream deleted long ago,
// and with nothing hidden.
func TestMirrorFromBefore(t *testing.T) {
	x := newMirrorFixture(t)
	gone := x.push("main", "refs/heads/gone")
	tag := x.pushTag("v1", "main")
	x.git(x.work, "push", "-q", "origin", "main:refs/heads/merged")
	if err := os.MkdirAll(filepath.Dir(x.mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	x.git(x.tmp, "clone", "-q", "--bare", "--", x.bare, x.mirror)
	x.git(x.mirror, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*")
	x.git(x.mirror, "config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*")
	x.deleteUpstream("refs/heads/gone", "refs/tags/v1", "refs/heads/merged")
	x.git(x.mirror, "fetch", "-q", "--no-tags", "--", x.bare, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	dir := x.make()
	x.wantSame()
	x.wantKept(gone, tag)
	x.wantHeld(gone, tag)
	if err := x.r.Check(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if out := x.git(x.tmp, "ls-remote", x.mirror); strings.Contains(out, "hataori") {
		t.Fatalf("shown:\n%s", out)
	}
}

// The branch that HEAD names is deleted upstream: a workspace still has files.
func TestMirrorDefaultBranchGone(t *testing.T) {
	x := newMirrorFixture(t)
	first := x.git(x.work, "rev-parse", "main")
	x.make()
	// Renamed, as a chat service's page does it: HEAD names the new one.
	x.git(x.bare, "branch", "-m", "main", "trunk")
	dir := x.make()
	x.wantSame()
	x.wantKept()
	if got := x.git(dir, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Fatal(got)
	}
	if err := x.r.Check(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	// Upstream's HEAD names no branch: the mirror's stays.
	x.git(x.bare, "branch", "-m", "trunk", "other")
	x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/nowhere")
	dir = x.make()
	if got := x.git(dir, "rev-parse", "HEAD"); got != first {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/other")
	x.make()
	x.wantSame()
}

// git says that it made the kept refs, and did not.
func TestMirrorReadsBackWhatItKept(t *testing.T) {
	x := newMirrorFixture(t)
	gone := x.push("main", "refs/heads/gone")
	x.make()
	old := x.refs("refs/heads/gone")
	x.deleteUpstream("refs/heads/gone")
	restore := x.wrapGit(`case " $* " in *' update-ref '*) [ -e "$bin/first" ] || { : > "$bin/first"; exit 0; };; esac`)
	x.make()
	restore()
	if got := x.refs("refs/heads/gone"); !slices.Equal(got, old) {
		t.Fatalf("dropped: %v", got)
	}
	x.make()
	x.wantSame()
	x.wantKept(gone)
}

// A deleted branch was merged, and the branch that has it now cannot be
// moved: what was fetched of that stays until it can.
func TestMirrorBranchCannotBeMoved(t *testing.T) {
	x := newMirrorFixture(t)
	merged := x.push("main", "refs/heads/merged")
	x.make()
	next := x.push(merged, "refs/heads/main")
	x.deleteUpstream("refs/heads/merged")
	lock := filepath.Join(x.mirror, "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	x.make()
	if got := x.refs("refs/heads/merged"); got != nil {
		t.Fatalf("still there: %v", got)
	}
	x.wantKept(next)
	x.wantHeld(merged)
	os.Remove(lock)
	x.push("main", "refs/heads/main")
	x.make()
	x.wantSame()
	x.wantHeld(merged)
}
