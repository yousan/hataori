package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	// What one update fetched from upstream, in a directory of its own. It
	// holds every object of that update until the branches and tags do.
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
	if err := g.replace(ctx, repo); err != nil {
		// Without "+" and --prune: no ref is deleted, and none is moved to
		// what does not reach its old place.
		_, _ = g.git(ctx, "", "fetch", "--no-tags", "--no-prune", "--no-prune-tags", "--show-forced-updates", "--", repo,
			"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	}
	g.clear(ctx)
	// Not by a fetch above, whose objects are not yet on the branches.
	_, _ = g.git(ctx, "", "maintenance", "run", "--auto", "--quiet")
}

// replace is update when nothing fails. At each point of it every object that
// a ref of the mirror named before is reachable from its refs: from what it
// was on, from a kept ref, or from what was just fetched.
func (g mirrorRepo) replace(ctx context.Context, repo string) error {
	if err := g.hide(ctx); err != nil {
		return err
	}
	var b [8]byte
	rand.Read(b[:])
	stage := fetchedRefs + hex.EncodeToString(b[:]) + "/"
	if _, err := g.git(ctx, "", "fetch", "--no-tags", "--no-prune", "--no-prune-tags", "--no-auto-gc", "--no-write-fetch-head", "--", repo,
		"+refs/heads/*:"+stage+"heads/*", "+refs/tags/*:"+stage+"tags/*"); err != nil {
		return err
	}
	refs, err := g.refs(ctx)
	if err != nil {
		return err
	}
	want := map[string]string{}
	var left []string // what the refs left reach
	for _, r := range refs {
		if r.symref {
			continue
		}
		if name, ok := strings.CutPrefix(r.name, stage); ok {
			want["refs/"+name] = r.id
			left = append(left, r.id)
		} else if strings.HasPrefix(r.name, keptRefs) {
			left = append(left, r.id)
		}
	}
	oldHead, newHead, err := g.head(ctx, repo, want)
	if err != nil {
		return err
	}
	var dropped []mirrorRef
	var deletes, updates []string
	have := map[string]bool{}
	for _, r := range refs {
		if !r.upstream() {
			continue
		}
		have[r.name] = true
		id, ok := want[r.name]
		switch {
		case ok && id == r.id:
		case r.symref: // not one that a fetch made
		case ok:
			dropped = append(dropped, r)
			updates = append(updates, "update "+r.name+" "+id+" "+r.id)
		case r.name == oldHead && newHead == "":
		default:
			dropped = append(dropped, r)
			if r.name != oldHead {
				deletes = append(deletes, "delete "+r.name+" "+r.id)
			}
		}
	}
	for name, id := range want {
		if !have[name] {
			updates = append(updates, "create "+name+" "+id)
		}
	}
	if err := g.keep(ctx, dropped, left); err != nil {
		return err
	}
	// The deletions first: upstream may have a/b in place of a. A line counts
	// only for the value that was looked at.
	g.change(ctx, deletes)
	g.change(ctx, updates)
	if newHead != "" {
		g.moveHead(ctx, oldHead, newHead, refs)
	}
	return nil
}

// hide sets the mirror to show none of hataori's own refs to who reads from
// it or pushes to it.
func (g mirrorRepo) hide(ctx context.Context) error {
	const key = "transfer.hideRefs"
	hidden := func() bool {
		out, err := g.git(ctx, "", "config", "--file", filepath.Join(g.dir, "config"), "--get-all", key)
		return err == nil && slices.Contains(strings.Fields(out), ownRefs)
	}
	if hidden() {
		return nil
	}
	if _, err := g.git(ctx, "", "config", "--file", filepath.Join(g.dir, "config"), "--add", key, ownRefs); err != nil {
		return err
	}
	if !hidden() {
		return errors.New("workspace: the mirror's own refs are not hidden")
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
	if _, err := g.git(ctx, "", "rev-parse", "--verify", "--quiet", now); err != nil {
		return // it could not be made
	}
	if _, err := g.git(ctx, "", "symbolic-ref", "HEAD", now); err != nil {
		return
	}
	for _, r := range before {
		if r.name == old && !r.symref {
			g.change(ctx, []string{"delete " + r.name + " " + r.id})
		}
	}
}

// keep gives each object in dropped that left does not reach a kept ref, and
// proves that it has one.
func (g mirrorRepo) keep(ctx context.Context, dropped []mirrorRef, left []string) error {
	var commits, others []string
	for _, r := range dropped {
		if r.kind == "commit" {
			commits = append(commits, r.id)
		} else {
			others = append(others, r.id)
		}
	}
	// A tag is kept as itself; what it names is then reached by it.
	ids, _, err := g.unreached(ctx, others, left)
	if err != nil {
		return err
	}
	lost, parents, err := g.unreached(ctx, commits, append(slices.Clip(left), ids...))
	if err != nil {
		return err
	}
	for _, id := range lost {
		if !parents[id] { // nor a commit that another one reaches
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var lines []string
	for _, id := range ids {
		lines = append(lines, "create "+keptRefs+id+" "+id)
	}
	if err := g.updateRefs(ctx, lines); err != nil {
		return err
	}
	refs, err := g.refs(ctx)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, r := range refs {
		kept[r.name] = !r.symref && r.name == keptRefs+r.id
	}
	for _, id := range ids {
		if !kept[keptRefs+id] {
			return fmt.Errorf("workspace: %s is not kept in the mirror", id)
		}
	}
	return nil
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
	// Of trees and blobs only those that are given.
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

// clear removes what updates fetched, this one's and what one that died left,
// when the other refs hold it.
func (g mirrorRepo) clear(ctx context.Context) {
	refs, err := g.refs(ctx)
	if err != nil {
		return
	}
	var fetched []mirrorRef
	var left, deletes []string
	held := map[string]bool{}
	for _, r := range refs {
		if strings.HasPrefix(r.name, fetchedRefs) {
			fetched = append(fetched, r)
			deletes = append(deletes, "delete "+r.name+" "+r.id)
		} else if !r.symref && (r.upstream() || strings.HasPrefix(r.name, keptRefs)) {
			left = append(left, r.id)
			held[r.id] = true
		}
	}
	fetched = slices.DeleteFunc(fetched, func(r mirrorRef) bool { return held[r.id] })
	if g.keep(ctx, fetched, left) == nil {
		g.change(ctx, deletes)
	}
}

// change makes the changes of refs together, or, when that fails, each that
// can be made.
func (g mirrorRepo) change(ctx context.Context, lines []string) {
	if len(lines) == 0 || g.updateRefs(ctx, lines) == nil || len(lines) == 1 {
		return
	}
	for _, line := range lines {
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
