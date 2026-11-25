package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The refs of a mirror that are hataori's own. The mirror's config hides
// them, so that no clone, fetch or push sees one.
const (
	ownRefs = "refs/hataori"
	// A kept ref names an object that the mirror's branches and tags no
	// longer reach since upstream deleted or moved one: the mirror may be
	// the only place of it. It is named by the object's id, and nothing
	// moves or deletes it.
	keptRefs = ownRefs + "/kept/"
	// A fetched ref names what a branch or tag is about to be moved to, until
	// one of those or a kept ref holds it.
	fetchedRefs = ownRefs + "/fetched/"
)

// mirrorRepo is a mirror whose lock is held.
type mirrorRepo struct {
	dir string
	env []string
}

// update makes the mirror's branches and tags those of repo. What one that
// repo deleted or moved named stays in the mirror, by a kept ref when the
// refs left do not reach it. When that cannot be done nothing is dropped: the
// mirror gets only what can be added to it.
func (g mirrorRepo) update(ctx context.Context, repo string) {
	guarded := g.guard(ctx)
	// Without "+" and --prune: no ref is deleted, and none is moved to what
	// does not reach its old place. One that it cannot move or make is an
	// error, and the objects for it are there all the same.
	_, _ = g.git(ctx, "", "fetch", "--no-tags", "--no-prune", "--no-prune-tags", "--show-forced-updates", "--", repo,
		"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	if guarded != nil {
		return
	}
	_ = g.replace(ctx, repo)
	g.clear(ctx)
}

// replace does what the fetch could not: it deletes the branches and tags that
// repo has no more, and moves those that it moved elsewhere. At each point
// every object that a ref of the mirror named before is reachable from its
// refs. When repo has something that was not fetched, as when it moved since,
// nothing is dropped.
func (g mirrorRepo) replace(ctx context.Context, repo string) error {
	out, err := g.git(ctx, "", "ls-remote", "--heads", "--tags", "--", repo)
	if err != nil {
		return err
	}
	want := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if id, name, ok := strings.Cut(line, "\t"); ok && !strings.HasSuffix(name, "^{}") {
			want[name] = id
		}
	}
	refs, err := g.refs(ctx)
	if err != nil {
		return err
	}
	oldHead, newHead, err := g.head(ctx, repo, want)
	if err != nil {
		return err
	}
	var dropped []mirrorRef
	var deletes, updates []string
	have, stays := map[string]bool{}, map[string]bool{}
	for _, r := range refs {
		if !r.upstream() {
			if !r.symref && strings.HasPrefix(r.name, keptRefs) {
				stays[r.id] = true
			}
			continue
		}
		have[r.name] = true
		id, ok := want[r.name]
		switch {
		case r.symref: // not one that a fetch made
		case ok && id == r.id:
			stays[r.id] = true
		case ok:
			dropped = append(dropped, r)
			updates = append(updates, "update "+r.name+" "+id+" "+r.id)
		case r.name == oldHead && newHead == "":
			stays[r.id] = true
		default:
			dropped = append(dropped, r)
			if r.name != oldHead {
				deletes = append(deletes, "delete "+r.name+" "+r.id)
			}
		}
	}
	var fetched []string
	for name, id := range want {
		if !have[name] {
			updates = append(updates, "create "+name+" "+id)
		}
		if !stays[id] {
			stays[id] = true
			fetched = append(fetched, id)
		}
	}
	// What git does before it makes a ref of what it fetched.
	if len(fetched) > 0 {
		if _, err := g.git(ctx, strings.Join(fetched, "\n")+"\n", "rev-list", "--objects", "--quiet", "--stdin", "--not", "--all"); err != nil {
			return err
		}
	}
	if len(dropped) > 0 {
		if err := g.hold(ctx, fetchedRefs, fetched); err != nil {
			return err
		}
		lost, err := g.lost(ctx, dropped, slices.Collect(maps.Keys(stays)))
		if err != nil {
			return err
		}
		if err := g.hold(ctx, keptRefs, lost); err != nil {
			return err
		}
	}
	// The deletions first: upstream may have a/b in place of a. A line counts
	// only for the value that was looked at.
	if newHead != "" && inTheWay(oldHead, want) {
		// No branch can be where the old one is, so HEAD goes first.
		g.moveHead(ctx, oldHead, newHead, refs)
		newHead = ""
	}
	g.change(ctx, deletes)
	g.change(ctx, updates)
	if newHead != "" {
		if _, err := g.git(ctx, "", "rev-parse", "--verify", "--quiet", newHead); err != nil {
			return nil // it could not be made
		}
		g.moveHead(ctx, oldHead, newHead, refs)
	}
	return nil
}

// inTheWay reports whether a ref of want is one that cannot be made where name
// is, as a/b where a is.
func inTheWay(name string, want map[string]string) bool {
	for other := range want {
		if strings.HasPrefix(other, name+"/") || strings.HasPrefix(name, other+"/") {
			return true
		}
	}
	return false
}

// guard sets the mirror to show none of hataori's own refs to who reads from
// it or pushes to it, and its gc to drop no object. A gc does not read all
// refs at once: it may see neither the branch that is then deleted nor the
// kept ref made before that.
func (g mirrorRepo) guard(ctx context.Context) error {
	for _, c := range []struct{ key, value, add string }{
		{"transfer.hideRefs", ownRefs, "--add"},
		{"gc.pruneExpire", "never", "--replace-all"},
	} {
		set := func() bool {
			out, err := g.git(ctx, "", "config", "--file", filepath.Join(g.dir, "config"), "--get-all", c.key)
			values := strings.Fields(out)
			if c.add != "--add" && len(values) > 1 {
				values = values[len(values)-1:]
			}
			return err == nil && slices.Contains(values, c.value)
		}
		if set() {
			continue
		}
		if _, err := g.git(ctx, "", "config", "--file", filepath.Join(g.dir, "config"), c.add, c.key, c.value); err != nil {
			return err
		}
		if !set() {
			return errors.New("workspace: the mirror's " + c.key + " cannot be set")
		}
	}
	return nil
}

// head is the branch that the mirror's HEAD names, when repo has it no
// longer, and the one that repo's HEAD names, or "" when that cannot be told:
// a mirror whose HEAD names no branch is cloned without a working tree.
func (g mirrorRepo) head(ctx context.Context, repo string, want map[string]string) (old, now string, err error) {
	old, err = g.git(ctx, "", "symbolic-ref", "--quiet", "HEAD")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", "", nil // detached
	}
	if err != nil {
		return "", "", err
	}
	if old = strings.TrimSpace(old); want[old] != "" {
		return "", "", nil
	}
	out, err := g.git(ctx, "", "ls-remote", "--symref", "--", repo, "HEAD")
	if err != nil {
		return old, "", nil
	}
	for _, line := range strings.Split(out, "\n") {
		target, ok := strings.CutPrefix(line, "ref: ")
		if target, isHead := strings.CutSuffix(target, "\tHEAD"); ok && isHead && strings.HasPrefix(target, "refs/heads/") && want[target] != "" {
			return old, target, nil
		}
	}
	return old, "", nil
}

// moveHead points HEAD to the branch now and then deletes old, which it named.
func (g mirrorRepo) moveHead(ctx context.Context, old, now string, before []mirrorRef) {
	if _, err := g.git(ctx, "", "symbolic-ref", "HEAD", now); err != nil {
		return
	}
	for _, r := range before {
		if r.name == old && !r.symref {
			g.change(ctx, []string{"delete " + r.name + " " + r.id})
		}
	}
}

// hold makes a ref below prefix for each of the ids, and proves that it is
// there. It makes refs only, and never changes one.
func (g mirrorRepo) hold(ctx context.Context, prefix string, ids []string) error {
	held := func() (map[string]bool, error) {
		refs, err := g.refs(ctx)
		have := map[string]bool{}
		for _, r := range refs {
			if !r.symref && strings.HasPrefix(r.name, prefix) {
				have[r.id] = true
			}
		}
		return have, err
	}
	// A second name, for when the first cannot be made: a git that died leaves
	// it locked.
	for _, suffix := range []string{"", "-" + rand.Text()} {
		have, err := held()
		if err != nil {
			return err
		}
		var lines []string
		for _, id := range ids {
			if !have[id] {
				lines = append(lines, "create "+prefix+id+suffix+" "+id)
			}
		}
		if len(lines) == 0 {
			return nil
		}
		_ = g.updateRefs(ctx, lines)
	}
	have, err := held()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !have[id] {
			return fmt.Errorf("workspace: %s is not held in the mirror", id)
		}
	}
	return nil
}

// lost are the ids of those of refs that left does not reach, without commits
// that another one of them reaches.
func (g mirrorRepo) lost(ctx context.Context, refs []mirrorRef, left []string) ([]string, error) {
	var commits, tags, others []string
	for _, r := range refs {
		switch r.kind {
		case "commit":
			commits = append(commits, r.id)
		case "tag":
			tags = append(tags, r.id)
		default:
			others = append(others, r.id)
		}
	}
	// A tag is kept as itself; what it names is then reached by it.
	ids, _, err := g.unreached(ctx, tags, left)
	if err != nil {
		return nil, err
	}
	gone, parents, err := g.unreached(ctx, commits, slices.Concat(left, ids))
	if err != nil {
		return nil, err
	}
	for _, id := range gone {
		if !parents[id] {
			ids = append(ids, id)
		}
	}
	if len(others) == 0 {
		return ids, nil
	}
	// A tree or blob may be in any commit, which takes longer to tell.
	out, err := g.git(ctx, strings.Join(slices.Concat(left, ids), "\n")+"\n", "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	for _, id := range strings.Fields(out) {
		reached[id] = true
	}
	for _, id := range others {
		if !reached[id] {
			ids = append(ids, id)
			reached[id] = true // once
		}
	}
	return ids, nil
}

// unreached are those of the ids that left does not reach, and the parents of
// all commits that only the ids reach.
func (g mirrorRepo) unreached(ctx context.Context, ids, left []string) (lost []string, parents map[string]bool, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	stdin := strings.Join(ids, "\n") + "\n"
	for _, id := range left {
		stdin += "^" + id + "\n"
	}
	// Commits and tags only, and the trees that are given.
	out, err := g.git(ctx, stdin, "rev-list", "--objects", "--filter=tree:0", "--no-object-names", "--parents", "--stdin")
	if err != nil {
		return nil, nil, err
	}
	listed := map[string]bool{}
	parents = map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		listed[f[0]] = true
		for _, p := range f[1:] {
			parents[p] = true
		}
	}
	for _, id := range ids {
		if listed[id] {
			lost = append(lost, id)
			delete(listed, id) // once
		}
	}
	return lost, parents, nil
}

// clear removes the fetched refs, this update's and what one that died left,
// that the branches, tags and kept refs reach. Others stay until they do.
func (g mirrorRepo) clear(ctx context.Context) {
	refs, err := g.refs(ctx)
	if err != nil {
		return
	}
	var fetched []mirrorRef
	var left, deletes []string
	for _, r := range refs {
		if strings.HasPrefix(r.name, fetchedRefs) {
			fetched = append(fetched, r)
		} else if !r.symref && (r.upstream() || strings.HasPrefix(r.name, keptRefs)) {
			left = append(left, r.id)
		}
	}
	if len(fetched) == 0 {
		return
	}
	lost, err := g.lost(ctx, fetched, left)
	if err != nil {
		return
	}
	for _, r := range fetched {
		if !slices.Contains(lost, r.id) {
			deletes = append(deletes, "delete "+r.name+" "+r.id)
		}
	}
	g.change(ctx, deletes)
}

// change makes the changes of refs together, or, when that fails, each that
// can be made, as long as that does not take long: git waits for a lock that
// one that died left.
func (g mirrorRepo) change(ctx context.Context, lines []string) {
	if len(lines) == 0 || g.updateRefs(ctx, lines) == nil || len(lines) == 1 {
		return
	}
	end := time.Now().Add(5 * time.Second)
	for _, line := range lines {
		if time.Now().After(end) {
			return
		}
		_ = g.updateRefs(ctx, []string{line})
	}
}

func (g mirrorRepo) updateRefs(ctx context.Context, lines []string) error {
	// A dying git commits what it read: no line may be half there.
	_, err := g.git(ctx, "start\n"+strings.Join(lines, "\n")+"\nprepare\ncommit\n", "-c", "core.fsync=all", "update-ref", "--no-deref", "--stdin")
	return err
}

// mirrorRef is one ref of a mirror.
type mirrorRef struct {
	name, id, kind string
	symref         bool
}

// upstream reports whether it is in a place that upstream's refs come to.
func (r mirrorRef) upstream() bool {
	return strings.HasPrefix(r.name, "refs/heads/") || strings.HasPrefix(r.name, "refs/tags/")
}

func (g mirrorRepo) refs(ctx context.Context) ([]mirrorRef, error) {
	out, err := g.git(ctx, "", "for-each-ref", "--format=%(objectname) %(objecttype) %(refname) %(symref)")
	if err != nil {
		return nil, err
	}
	var refs []mirrorRef
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, " ")
		if len(f) != 4 {
			return nil, fmt.Errorf("workspace: the mirror's ref %q", line)
		}
		refs = append(refs, mirrorRef{id: f[0], kind: f[1], name: f[2], symref: f[3] != ""})
	}
	return refs, nil
}

// git runs git in the mirror and returns what it printed.
func (g mirrorRepo) git(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := gitCommand(ctx, append([]string{"--git-dir", g.dir}, args...)...)
	cmd.Env = append(slices.Clip(g.env), "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE="+os.DevNull, "LC_ALL=C")
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("workspace: the mirror: git %s: %s (%w)", args[0], lastLines(redact(stderr.String()), 3, ""), err)
	}
	return stdout.String(), nil
}
