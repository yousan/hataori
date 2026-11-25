package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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
func (x *mirrorFixture) refs(prefixes ...string) []string {
	x.t.Helper()
	out := x.git(x.mirror, append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, prefixes...)...)
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
	// One that a commit has is not, however old the commit.
	x.git(x.work, "read-tree", "main")
	x.git(x.work, "update-index", "--add", "--cacheinfo", "100644,"+lone+",more")
	other := x.git(x.work, "write-tree")
	x.git(x.work, "read-tree", "main")
	older := x.git(x.work, "commit-tree", "-p", next, "-m", "another tree", other)
	next = x.push(older, "refs/heads/main")
	x.git(x.work, "push", "-q", "origin", other+":refs/tags/old-tree", next+"^{tree}:refs/tags/new-tree", blob+":refs/tags/old-blob")
	x.make()
	x.deleteUpstream("refs/tags/old-tree", "refs/tags/new-tree", "refs/tags/old-blob")
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
		"git makes none": func(x *mirrorFixture, id string) {
			x.wrapGit(`case " $* " in *' update-ref '*)
				in=$(cat); case "$in" in *'create refs/hataori/'*) exit 0;; esac
				printf '%s\n' "$in" | "$real" "$@"; exit;;
			esac`)
		},
		"the gc cannot be told": func(x *mirrorFixture, id string) {
			x.git(x.mirror, "config", "--unset-all", "gc.pruneExpire")
			os.WriteFile(filepath.Join(x.mirror, "config.lock"), nil, 0o600)
		},
		"the refs cannot be hidden": func(x *mirrorFixture, id string) {
			x.git(x.mirror, "config", "--unset-all", "transfer.hideRefs")
			os.WriteFile(filepath.Join(x.mirror, "config.lock"), nil, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newMirrorFixture(t)
			gone := x.push("main", "refs/heads/gone")
			x.push("main", "refs/heads/moved", "refs/tags/gone")
			x.make()
			x.make()
			// With this a fetch that is not told otherwise moves a branch anywhere.
			x.git(x.mirror, "config", "fetch.showForcedUpdates", "false")
			x.git(x.mirror, "config", "fetch.prune", "true")
			x.git(x.mirror, "config", "fetch.pruneTags", "true")
			block(x, gone)
			old := x.refs("refs/heads/gone", "refs/heads/moved", "refs/tags")
			had := x.refs(keptRefs)
			x.deleteUpstream("refs/heads/gone", "refs/tags/gone")
			x.push("main", "refs/heads/moved")
			added := x.push("main", "refs/heads/added")
			x.make()
			if got := x.refs("refs/heads/gone", "refs/heads/moved", "refs/tags"); !slices.Equal(got, old) {
				t.Fatalf("dropped: %v, want %v", got, old)
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

// The name of a kept or fetched ref cannot be had, as when a git that died
// left it locked: the object gets another.
func TestMirrorKeepsByAnotherName(t *testing.T) {
	for name, block := range map[string]func(x *mirrorFixture, ref string){
		"locked": func(x *mirrorFixture, ref string) {
			p := filepath.Join(x.mirror, ref+".lock")
			if err := errors.Join(os.MkdirAll(filepath.Dir(p), 0o700), os.WriteFile(p, nil, 0o600)); err != nil {
				x.t.Fatal(err)
			}
		},
		"of another object": func(x *mirrorFixture, ref string) { x.git(x.mirror, "update-ref", ref, "refs/heads/main") },
		"symbolic":          func(x *mirrorFixture, ref string) { x.git(x.mirror, "symbolic-ref", ref, "refs/heads/gone") },
	} {
		t.Run(name, func(t *testing.T) {
			x := newMirrorFixture(t)
			gone := x.push("main", "refs/heads/gone")
			old := x.push("main", "refs/heads/amended")
			x.make()
			x.deleteUpstream("refs/heads/gone")
			amended := x.push("main", "refs/heads/amended")
			block(x, keptRefs+gone)
			block(x, fetchedRefs+amended)
			had := x.refs(ownRefs)
			x.make()
			if name == "of another object" {
				for _, l := range had {
					if strings.Contains(l, keptRefs) && !slices.Contains(x.refs(ownRefs), l) {
						t.Fatalf("%s is gone or names another: %v", l, x.refs(ownRefs))
					}
				}
			}
			x.wantHeld(gone, old)
			if got := strings.Join(x.refs("refs/heads/"), "\n"); strings.Contains(got, "gone") || !strings.Contains(got, amended) {
				t.Fatal(got)
			}
			n := 0
			for _, l := range x.refs(keptRefs) {
				if strings.HasPrefix(l, gone) || strings.HasPrefix(l, old) {
					n++
				}
			}
			if n != 2 {
				t.Fatal(x.refs(keptRefs))
			}
		})
	}
}

// A git that died left packed-refs locked: the update ends, and a later one
// does what it could not.
func TestMirrorPackedRefsLocked(t *testing.T) {
	x := newMirrorFixture(t)
	ids := x.drop()
	for i := range 20 {
		x.git(x.mirror, "update-ref", fmt.Sprint("refs/heads/many", i), "main")
	}
	lock := filepath.Join(x.mirror, "packed-refs.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	x.make()
	if d := time.Since(start); d > 20*time.Second {
		t.Fatal(d)
	}
	os.Remove(lock)
	x.wantHeld(ids...)
	x.make()
	x.wantSame()
	x.wantHeld(ids...)
}

// Upstream moves on during an update. When that is after what the mirror
// fetched and before it looks what upstream has, nothing is dropped.
func TestMirrorUpstreamMovesMeanwhile(t *testing.T) {
	x := newMirrorFixture(t)
	ids := x.drop()
	x.push("main", "refs/heads/second")
	x.make()
	for call := 1; call <= 12; call++ {
		next := x.push("main")
		move := func(id string) string {
			return fmt.Sprintf("\"$real\" -C '%s' push -q -f origin %s:refs/heads/main %s:refs/heads/amended >/dev/null 2>&1", x.work, id, id)
		}
		restore := x.wrapGit(fmt.Sprintf("[ $n -eq %d ] && { %s; }", call, strings.Replace(move(next), " >", " :refs/heads/second >", 1)))
		before := x.git(x.mirror, "for-each-ref", "--format=%(objectname)")
		mirrorRepo{dir: x.mirror, env: x.m.gitEnv()}.update(context.Background(), "file://"+x.bare)
		restore()
		x.wantHeld(append(strings.Fields(before), ids...)...)
		x.push("main", "refs/heads/second")
		x.make()
		x.wantSame()
		x.deleteUpstream("refs/heads/second")
		next = x.push("main")
		old := x.refs("refs/heads/")
		restore = x.wrapGit(`case " $* " in *' ls-remote '*) [ -e "$bin/first" ] || { : > "$bin/first"; ` + move(next) + `; };; esac`)
		mirrorRepo{dir: x.mirror, env: x.m.gitEnv()}.update(context.Background(), "file://"+x.bare)
		restore()
		if got := x.refs("refs/heads/"); !slices.Equal(got, old) {
			t.Fatalf("dropped:\n%s\nit was\n%s", strings.Join(got, "\n"), strings.Join(old, "\n"))
		}
		x.push("main", "refs/heads/second")
		x.make()
		x.wantSame()
	}
}

// As above, where upstream moves to what the mirror has already.
func TestMirrorUpstreamMovesBackMeanwhile(t *testing.T) {
	x := newMirrorFixture(t)
	first := x.git(x.work, "rev-parse", "main")
	x.push("main", "refs/heads/gone")
	x.push(x.push("main"), "refs/heads/back", "refs/heads/it's", "refs/tags/branch")
	x.make()
	x.make()
	old := x.refs("refs/heads", "refs/tags")
	restore := x.wrapGit(fmt.Sprintf(`case " $* " in *' ls-remote '*) [ -e "$bin/first" ] || { : > "$bin/first"; "$real" -C '%s' push -q -f origin %s:refs/heads/back :refs/heads/gone >/dev/null 2>&1; };; esac`, x.work, first))
	x.make()
	restore()
	if got := x.refs("refs/heads", "refs/tags"); !slices.Equal(got, old) {
		t.Fatalf("dropped:\n%s\nit was\n%s", strings.Join(got, "\n"), strings.Join(old, "\n"))
	}
	x.make()
	x.wantSame()
}

// A gc of the mirror drops no object, however old.
func TestMirrorGCDropsNothing(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	x.make()
	old := time.Now().AddDate(-2, 0, 0)
	x.push("main", "refs/heads/x")
	id := x.git(x.mirror, "commit-tree", "-m", "no ref names it", "main^{tree}")
	p := filepath.Join(x.mirror, "objects", id[:2], id[2:])
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		x.git(x.mirror, "gc", "-q")
		x.git(x.mirror, "cat-file", "-e", id)
	}
}

// git fetches from a shallow upstream with no word of failure where it does
// not take a branch: one that upstream has is not deleted.
func TestMirrorOfShallowUpstream(t *testing.T) {
	x := newMirrorFixture(t)
	x.push(x.push("main"), "refs/heads/x")
	full := filepath.Join(x.tmp, "full.git")
	if err := os.Rename(x.bare, full); err != nil {
		t.Fatal(err)
	}
	x.git(x.tmp, "clone", "-q", "--bare", "--depth=1", "--no-single-branch", "file://"+full, x.bare)
	x.make()
	old := x.refs("refs/heads/x")
	x.git(x.work, "remote", "set-url", "origin", full)
	x.push(x.push("origin/main"), "refs/heads/x")
	x.git(x.bare, "fetch", "-q", "--depth=1", "file://"+full, "+refs/heads/x:refs/heads/x")
	x.make()
	if got := x.refs("refs/heads/x"); len(got) != 1 || len(old) != 1 {
		t.Fatalf("x is %v, it was %v", got, old)
	}
}

// What the mirror says of an object's parents is not what it is.
func TestMirrorKeepsWhatReplacedParentsReach(t *testing.T) {
	x := newMirrorFixture(t)
	gone := x.push("main", "refs/heads/gone")
	grafted := x.push("main", "refs/heads/grafted")
	main := x.push("main", "refs/heads/main")
	x.make()
	x.make()
	x.git(x.mirror, "replace", main, x.git(x.mirror, "commit-tree", "-m", "replaced", "-p", gone, main+"^{tree}"))
	if err := os.WriteFile(filepath.Join(x.mirror, "info", "grafts"), []byte(main+" "+grafted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.deleteUpstream("refs/heads/gone", "refs/heads/grafted")
	x.make()
	x.wantKept(gone, grafted)
}

// The reflog of a branch or tag, of a mirror that has reflogs, goes with it.
func TestMirrorKeepsWhatAReflogNames(t *testing.T) {
	x := newMirrorFixture(t)
	first := x.push("main", "refs/heads/gone")
	tag := x.pushTag("v1", "main")
	x.pushTag("v2", "main")
	x.make()
	x.make()
	x.git(x.mirror, "config", "core.logAllRefUpdates", "always")
	second, third := x.push("main", "refs/heads/gone"), ""
	for _, id := range []*string{&second, &third} {
		*id = x.push("main", "refs/heads/gone")
		x.git(x.mirror, "fetch", "-q", "file://"+x.bare, "+refs/heads/gone:refs/heads/gone")
	}
	x.git(x.mirror, "update-ref", "refs/tags/v1", "refs/heads/main")
	x.git(x.mirror, "update-ref", "refs/tags/v1", "refs/tags/v2")
	log := filepath.Join(x.mirror, "logs", "refs", "heads", "gone")
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// What is gone already, what is no id, and what no later entry comes after.
	nothing := strings.Repeat("12", 20)
	last := x.git(x.mirror, "commit-tree", "-m", "last", "refs/heads/main^{tree}")
	if err := os.WriteFile(log, []byte(nothing+" "+nothing+" x\n\nx\n"+nothing+" "+last+" x\n"+string(b)), 0o600); err != nil {
		t.Fatal(err)
	}
	// What a git that died left of the log of plain/b.
	x.push("main", "refs/heads/plain")
	x.git(x.mirror, "-c", "core.logAllRefUpdates=false", "fetch", "-q", "file://"+x.bare, "refs/heads/plain:refs/heads/plain")
	if err := os.MkdirAll(filepath.Join(x.mirror, "logs", "refs", "heads", "plain"), 0o700); err != nil {
		t.Fatal(err)
	}
	x.deleteUpstream("refs/heads/gone", "refs/heads/plain", "refs/tags/v1", "refs/tags/v2")
	x.push("main", "refs/tags/light")
	x.make()
	x.wantSame()
	x.wantHeld(first, second, third, last, tag)
}

// In a mirror with a reftable, a ref that has a reflog stays.
func TestMirrorKeepsRefsWithReflogsInAReftable(t *testing.T) {
	x := newMirrorFixture(t)
	first := x.push("main", "refs/heads/gone")
	x.push("main", "refs/heads/plain")
	x.make()
	x.make()
	if out, err := exec.Command("git", "-C", x.mirror, "refs", "migrate", "--ref-format=reftable").CombinedOutput(); err != nil {
		t.Skipf("this git makes no reftable: %.60s", out)
	}
	x.git(x.mirror, "config", "core.logAllRefUpdates", "true")
	x.push("main", "refs/heads/gone")
	x.git(x.mirror, "fetch", "-q", "file://"+x.bare, "+refs/heads/gone:refs/heads/gone")
	x.git(x.mirror, "symbolic-ref", "HEAD", "refs/heads/gone")
	x.deleteUpstream("refs/heads/gone", "refs/heads/plain")
	x.make()
	if got := x.refs("refs/heads/gone", "refs/heads/plain"); len(got) != 1 || !strings.HasSuffix(got[0], "/gone") {
		t.Fatalf("left: %v", got)
	}
	x.wantHeld(first)
}

// What changed in the mirror since it was looked at is left.
func TestMirrorChangesOnlyWhatItSaw(t *testing.T) {
	x := newMirrorFixture(t)
	x.push("main", "refs/heads/gone", "refs/heads/moved")
	x.make()
	x.make()
	other := x.git(x.mirror, "commit-tree", "-m", "other", "main^{tree}")
	x.deleteUpstream("refs/heads/gone")
	x.push("main", "refs/heads/moved")
	restore := x.wrapGit(`case " $* " in *' update-ref '*)
		if [ -n "$("$real" --git-dir '` + x.mirror + `' for-each-ref ` + keptRefs + `)" ] && [ ! -e "$bin/done" ]; then
			: > "$bin/done"
			for b in gone moved; do "$real" --git-dir '` + x.mirror + `' update-ref refs/heads/$b ` + other + `; done
		fi;;
	esac`)
	x.make()
	restore()
	for _, ref := range x.refs("refs/heads/gone", "refs/heads/moved") {
		if !strings.HasPrefix(ref, other) {
			t.Fatal(ref)
		}
	}
	if n := len(x.refs("refs/heads/gone", "refs/heads/moved")); n != 2 {
		t.Fatalf("%d branches are left", n)
	}
	x.make()
	x.wantSame()
	x.wantHeld(other)
}

// A git that gets a part of its orders, as when hataori dies, does nothing.
func TestMirrorHalfAChange(t *testing.T) {
	x := newMirrorFixture(t)
	x.push("main", "refs/heads/gone")
	x.make()
	x.make()
	g := mirrorRepo{dir: x.mirror, env: x.m.gitEnv()}
	old := x.refs()
	main := strings.Fields(x.refs("refs/heads/main")[0])[0]
	// Cut at the end of a name, where the rest is a check of the old value.
	for _, orders := range []string{"delete refs/heads/gone", "delete refs/heads/gone " + main + "\n"} {
		restore := x.wrapGit(fmt.Sprintf(`case " $* " in *' update-ref '*) head -c %d | "$real" "$@"; exit;; esac`, len("start\n"+orders)))
		g.change(context.Background(), []string{"delete refs/heads/gone " + main, "delete refs/heads/main " + main})
		restore()
		if got := x.refs(); !slices.Equal(got, old) {
			t.Fatalf("%q:\n%s\nit was\n%s", orders, strings.Join(got, "\n"), strings.Join(old, "\n"))
		}
	}
}

// A git that changes refs holds the mirror's lock when hataori is gone.
func TestMirrorLockOutlivesHataori(t *testing.T) {
	x := newMirrorFixture(t)
	x.drop()
	name := filepath.Join(x.tmp, "a.lock")
	open := func() *os.File {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	lock, other := open(), open()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(x.tmp, "pid")
	restore := x.wrapGit(`case " $* " in *' update-ref '*) [ -e '` + pid + `' ] || { sleep 60 </dev/null >/dev/null 2>&1 & echo $! > '` + pid + `'; };; esac`)
	mirrorRepo{dir: x.mirror, env: x.m.gitEnv(), lock: lock}.update(context.Background(), "file://"+x.bare)
	restore()
	lock.Close()
	held := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	b, err := os.ReadFile(pid)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("kill", strings.TrimSpace(string(b))).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if held != unix.EWOULDBLOCK {
		t.Fatalf("the lock is free: %v", held)
	}
}

// What the transport leaves behind does not make a git that did its work fail.
func TestMirrorTransportLeavesAProcess(t *testing.T) {
	x := newMirrorFixture(t)
	ids := x.drop()
	restore := x.wrapGit(`case " $* " in *' ls-remote '*) sleep 3 >/dev/null </dev/null & ;; esac`)
	x.make()
	restore()
	x.wantSame()
	x.wantHeld(ids...)
}

// The environment sets what the gc drops.
func TestMirrorGCWhateverTheEnvironment(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "gc.pruneExpire")
	t.Setenv("GIT_CONFIG_VALUE_0", "now")
	g := mirrorRepo{dir: x.mirror, env: append(os.Environ(), x.m.gitEnv()...)}
	if out, err := g.git(context.Background(), "", "config", "gc.pruneExpire"); err != nil || out != "never\n" {
		t.Fatalf("%q, %v", out, err)
	}
}

// More refs than one git is to change.
func TestMirrorOfManyDropped(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	var in strings.Builder
	var names []string
	n := refsAtOnce + 50
	for i := range n {
		name := fmt.Sprintf("refs/heads/b%03d", i)
		names = append(names, name)
		fmt.Fprintf(&in, "commit %s\ncommitter a <a@example.com> %d +0000\ndata 0\n\n", name, 1700000000+i)
	}
	cmd := exec.Command("git", "-C", x.bare, "fast-import", "--quiet")
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	x.make()
	var want []string
	for _, ref := range x.refs("refs/heads/b*") {
		want = append(want, strings.Fields(ref)[0])
	}
	if len(want) != n {
		t.Fatalf("%d branches came", len(want))
	}
	cmd = exec.Command("git", "-C", x.bare, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader("delete " + strings.Join(names, "\ndelete ") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	restore := x.wrapGit(fmt.Sprintf(`case " $* " in *' update-ref '*)
		cat > "$bin/in"; [ $(wc -l < "$bin/in") -le %d ] || exit 1
		exec "$real" "$@" < "$bin/in";;
	esac`, refsAtOnce+5))
	x.make()
	restore()
	x.wantSame()
	x.wantKept(want...)
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

// The same with the branch that HEAD names, both ways.
func TestMirrorDefaultBranchBecomesDirectory(t *testing.T) {
	x := newMirrorFixture(t)
	x.push("main", "refs/heads/main")
	x.make()
	var kept []string
	for _, b := range [][2]string{{"main", "main/v2"}, {"main/v2", "main"}} {
		kept = append(kept, x.git(x.bare, "rev-parse", "HEAD"))
		x.git(x.bare, "update-ref", "-d", "refs/heads/"+b[0])
		x.push("main", "refs/heads/"+b[1])
		x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/"+b[1])
		dir := x.make()
		x.wantSame()
		x.wantKept(kept...)
		if got := x.git(dir, "symbolic-ref", "HEAD"); got != "refs/heads/"+b[1] {
			t.Fatal(got)
		}
		if _, err := os.Stat(filepath.Join(dir, "a")); err != nil {
			t.Fatal(err)
		}
		if err := x.r.Check(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
	}
}

// What an update that died fetched stays as long as nothing else holds it.
func TestMirrorFetchedRefsLeft(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	gone := x.push("main", "refs/heads/gone")
	merged := x.push("main", "refs/heads/merged")
	x.git(x.mirror, "fetch", "-q", "--no-tags", x.bare, "refs/heads/gone:"+fetchedRefs+gone, "refs/heads/merged:"+fetchedRefs+merged)
	x.push(merged, "refs/heads/main")
	x.deleteUpstream("refs/heads/gone", "refs/heads/merged")
	x.make()
	if got := x.refs(fetchedRefs); len(got) != 1 || !strings.HasPrefix(got[0], gone) {
		t.Fatal(got)
	}
	x.wantHeld(gone, merged)
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
	x.wantKept()
	if got := x.refs(fetchedRefs); len(got) != 1 || !strings.HasPrefix(got[0], next) {
		t.Fatal(got)
	}
	os.Remove(lock)
	x.wantHeld(merged)
	x.make()
	x.wantSame()
	x.push("main", "refs/heads/main")
	x.make()
	x.wantSame()
	x.wantKept(next)
	x.wantHeld(merged)
}
