package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// TrashDir is where removed workspaces go, relative to the instance root.
const TrashDir = "trash"

// scratchDir holds the scratch repositories a check fetches into, relative
// to the instance root.
const scratchDir = "state/removal"

// trashStamp starts the name of every entry the removal puts in trash: the
// time it was moved there. The purge deletes nothing else.
const trashStamp = "20060102T150405Z"

// Reason says why a workspace may not be removed.
type Reason string

// The reasons a removal is refused.
const (
	NotWorkspace  Reason = "not a workspace"
	Unreadable    Reason = "unreadable directory"
	GitError      Reason = "git error"
	Timeout       Reason = "timeout"
	Modified      Reason = "modified file"
	Staged        Reason = "staged file"
	Untracked     Reason = "untracked file"
	Hidden        Reason = "file hidden from git status"
	Ignored       Reason = "ignored file not listed as disposable"
	Worktree      Reason = "linked worktree"
	UnpushedRef   Reason = "unpushed commit on a branch"
	DetachedHEAD  Reason = "unpushed commit at a detached HEAD"
	Stash         Reason = "stash"
	RemoteGone    Reason = "commit the remote no longer has"
	OtherRef      Reason = "unpushed commit on a ref"
	ReflogOnly    Reason = "commit only in a reflog"
	RemoteIsLocal Reason = "remote is another workspace"
	RemoteInRoot  Reason = "remote under a hataori root"
	// RemoteUnresolved is a local remote address that cannot be resolved
	// the way git resolves it.
	RemoteUnresolved Reason = "local remote address that cannot be resolved"
	InUse            Reason = "process using the directory"
	UnknownUse       Reason = "cannot tell whether a process uses the directory"
)

// Refusal is why a workspace may not be removed. Every error Check and
// Remove return is a *Refusal.
type Refusal struct {
	Reason Reason
	// Repo is where it was found, relative to the workspace: "" for the
	// workspace's own repository, else a submodule, a nested repository or
	// the git directory of a submodule with no checkout.
	Repo string
	// Kind names what Repo is: "", "submodule", "nested repository",
	// "nested bare repository" or "submodule without checkout".
	Kind   string
	Detail string
}

func (r *Refusal) Error() string {
	s := "workspace: removal refused: " + string(r.Reason)
	if r.Repo != "" {
		s += " in " + r.Kind + " " + r.Repo
	}
	if r.Detail != "" {
		s += ": " + r.Detail
	}
	return s
}

// Remover removes workspaces of one instance: it proves that a workspace
// holds nothing that exists nowhere else (design, section 4.6), moves a
// proven one into trash/, and purges trash/. It is the only code in
// hataori that deletes user data.
type Remover struct {
	// Maker gives the instance root and the daemon's environment.
	Maker Maker
	// Disposable lists the patterns of ignored files that are not work
	// (the config's cleanup.disposable). A pattern without a slash matches
	// any path segment; one with a slash matches the path from the top of
	// the repository, or a leading part of it. Both use path.Match.
	Disposable []string
	// OtherRoots are the other hataori roots on this host. A remote under
	// any of them, or under Maker.Root, does not count.
	OtherRoots []string
	// Retention is how long an entry stays in trash before the purge
	// deletes it. The purge refuses to run with zero.
	Retention time.Duration
	// Timeout bounds one check, every git and lsof run included; zero
	// means two minutes.
	Timeout time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	// objectLimit replaces objectEntryLimit when not 0, for tests.
	objectLimit int
}

func (r Remover) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Remove checks the workspace at dir and, when nothing in it would be
// lost, renames it into trash/ inside the instance root and returns where
// it went. The rename never replaces anything and never crosses file
// systems. Nothing else changes.
func (r Remover) Remove(ctx context.Context, dir string) (string, error) {
	abs, root, err := r.place(dir)
	if err != nil {
		return "", err
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return "", &Refusal{Reason: NotWorkspace, Detail: err.Error()}
	}
	if err := r.check(ctx, abs, root); err != nil {
		return "", err
	}
	trash, err := realDir(filepath.Join(root, TrashDir), true)
	if err != nil {
		return "", &Refusal{Reason: NotWorkspace, Detail: err.Error()}
	}
	// The directory checked is the one renamed: one put in its place
	// meanwhile is refused. What changes inside it after the check is
	// still in trash for the retention.
	after, err := os.Lstat(abs)
	if err != nil || !os.SameFile(before, after) {
		return "", &Refusal{Reason: NotWorkspace, Detail: abs + " was replaced during the check"}
	}
	var b [4]byte
	rand.Read(b[:])
	name := r.now().UTC().Format(trashStamp) + "-" + hex.EncodeToString(b[:]) + "-" + Segment(filepath.Base(abs))
	// First under a name the purge never takes: a directory that turns out
	// not to be the checked one and cannot be put back stays there.
	pending := filepath.Join(trash, pendingPrefix+name)
	if err := renameNoReplace(abs, pending); err != nil {
		return "", &Refusal{Reason: NotWorkspace, Detail: "rename into trash: " + err.Error()}
	}
	// A directory swapped in between the last look and the rename is put
	// back where it was.
	if moved, err := os.Lstat(pending); err != nil || !os.SameFile(before, moved) {
		if err := renameNoReplace(pending, abs); err != nil {
			return "", &Refusal{Reason: NotWorkspace, Detail: abs + " was replaced during the removal and is left at " + pending}
		}
		return "", &Refusal{Reason: NotWorkspace, Detail: abs + " was replaced during the removal"}
	}
	to := filepath.Join(trash, name)
	if err := renameNoReplace(pending, to); err != nil {
		return "", &Refusal{Reason: NotWorkspace, Detail: "the workspace is at " + pending + ": " + err.Error()}
	}
	return to, nil
}

// pendingPrefix starts the name of a workspace on its way into trash.
const pendingPrefix = "pending-"

// realDir returns dir when it is a directory itself, not a link to one,
// making it first when make is set.
func realDir(dir string, make bool) (string, error) {
	if make {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, nil
}

// trashName matches the names Remove gives: the time, eight hex digits and
// the workspace's last segment.
var trashName = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}-[A-Za-z0-9._%-]+$`)

// Purge deletes the entries of trash/ that were moved there longer than
// Retention ago, by the time in their names, and returns them. An entry
// whose name the removal did not make is left alone.
func (r Remover) Purge() ([]string, error) {
	if r.Retention <= 0 {
		return nil, errors.New("workspace: purge: no retention set")
	}
	root, err := filepath.EvalSymlinks(r.Maker.Root)
	if err != nil {
		return nil, fmt.Errorf("workspace: purge: %w", err)
	}
	trash, err := realDir(filepath.Join(root, TrashDir), false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: purge: %w", err)
	}
	// Everything below goes through the directory opened here, which a
	// later swap of the path does not change, and never out of it.
	seen, err := os.Lstat(trash)
	if err != nil {
		return nil, fmt.Errorf("workspace: purge: %w", err)
	}
	tr, err := os.OpenRoot(trash)
	if err != nil {
		return nil, fmt.Errorf("workspace: purge: %w", err)
	}
	defer tr.Close()
	if opened, err := tr.Stat("."); err != nil || !os.SameFile(seen, opened) {
		return nil, fmt.Errorf("workspace: purge: %s changed while it was opened", trash)
	}
	dir, err := tr.Open(".")
	if err != nil {
		return nil, fmt.Errorf("workspace: purge: %w", err)
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, fmt.Errorf("workspace: purge: %w", err)
	}
	now := r.now()
	var purged []string
	for _, e := range entries {
		name := e.Name()
		if !trashName.MatchString(name) {
			continue
		}
		moved, err := time.Parse(trashStamp, name[:len(trashStamp)])
		if err != nil || now.Sub(moved) <= r.Retention {
			continue
		}
		p := filepath.Join(trash, name)
		if err := tr.RemoveAll(name); err != nil {
			return purged, fmt.Errorf("workspace: purge: %w", err)
		}
		purged = append(purged, p)
	}
	return purged, nil
}

// Check answers whether the workspace at dir may be removed: nil when
// every step of section 4.6 is proven at this moment, else a *Refusal
// naming the first step that is not. A step that cannot be answered
// refuses. It changes nothing in dir.
func (r Remover) Check(ctx context.Context, dir string) error {
	abs, root, err := r.place(dir)
	if err != nil {
		return err
	}
	return r.check(ctx, abs, root)
}

func (r Remover) check(ctx context.Context, abs, root string) error {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := &checker{r: r, ctx: ctx, top: abs, root: root}
	err := c.run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return &Refusal{Reason: Timeout, Detail: ctx.Err().Error()}
	}
	var ref *Refusal
	if errors.As(err, &ref) {
		return ref
	}
	return &Refusal{Reason: GitError, Detail: err.Error()}
}

// place resolves dir and the root, and refuses a dir that is not a
// directory strictly inside the root's workspaces directory.
func (r Remover) place(dir string) (abs, root string, err error) {
	refuse := func(detail string) (string, string, error) {
		return "", "", &Refusal{Reason: NotWorkspace, Detail: detail}
	}
	if r.Maker.Root == "" {
		return refuse("no instance root")
	}
	if root, err = filepath.EvalSymlinks(r.Maker.Root); err != nil {
		return refuse(err.Error())
	}
	if abs, err = filepath.Abs(dir); err != nil {
		return refuse(err.Error())
	}
	st, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return "", "", &Refusal{Reason: Unreadable, Detail: err.Error()}
		}
		return refuse(err.Error())
	}
	if !st.IsDir() {
		return refuse(dir + " is not a directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return refuse(err.Error())
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	if !within(abs, filepath.Join(root, Dir)) {
		return refuse(dir + " is not inside the instance's workspaces directory")
	}
	return abs, root, nil
}

// within reports whether p is strictly inside dir. Both are clean.
func within(p, dir string) bool {
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}

// checker is one check of one workspace.
type checker struct {
	r    Remover
	ctx  context.Context
	top  string // the workspace, resolved
	root string // the instance root, resolved
	n    int    // scratch repositories made
}

// repo is a repository found in the workspace.
type repo struct {
	dir    string // its work tree, or its git directory when gitDir
	gitDir bool   // a git directory with no work tree here
	rel    string // relative to the workspace
	kind   string
}

func (c *checker) run() error {
	repos, err := c.find()
	if err != nil {
		return err
	}
	// Every work tree first: a dirty file is the most common reason, and
	// asking the remotes is the slowest step.
	readers := make([]*reader, len(repos))
	for i, rp := range repos {
		g, err := c.open(rp)
		if err != nil {
			return err
		}
		readers[i] = g
		if !rp.gitDir {
			if err := c.files(g, rp); err != nil {
				return err
			}
		}
	}
	for i, rp := range repos {
		if err := c.commits(readers[i], rp); err != nil {
			return err
		}
	}
	return c.processes()
}

func (c *checker) refuse(rp repo, reason Reason, detail string) error {
	return &Refusal{Reason: reason, Repo: rp.rel, Kind: rp.kind, Detail: detail}
}

// find walks the workspace and returns every repository in it: the
// workspace's own, each directory below it that has a .git or is a git
// directory itself (a bare repository), and each git directory under a
// repository's .git/modules, whether checked out or not. A directory it
// cannot read refuses.
func (c *checker) find() ([]repo, error) {
	var repos []repo
	err := filepath.WalkDir(c.top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return c.unreadable(p, err)
		}
		if d.IsDir() && p != c.top && d.Name() != ".git" && isGitDir(p) {
			rel, _ := filepath.Rel(c.top, p)
			repos = append(repos, repo{dir: p, gitDir: true, rel: rel, kind: "nested bare repository"})
			return filepath.SkipDir
		}
		if d.Name() != ".git" || p == c.top {
			return nil
		}
		dir := filepath.Dir(p)
		rel, _ := filepath.Rel(c.top, dir)
		rp := repo{dir: dir, rel: rel}
		switch {
		case rel == ".":
			rp.rel = ""
		case d.IsDir():
			rp.kind = "nested repository"
		default:
			rp.kind = "submodule"
		}
		repos = append(repos, rp)
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	topAt := -1
	for i, rp := range repos {
		if rp.rel == "" {
			topAt = i
		}
	}
	if topAt < 0 {
		return nil, &Refusal{Reason: NotWorkspace, Detail: c.top + " has no .git"}
	}
	repos[0], repos[topAt] = repos[topAt], repos[0]
	// The git directories of submodules, with or without a checkout. A
	// checked-out one is checked twice over, which costs only time.
	var modules []repo
	for _, rp := range repos {
		if rp.gitDir {
			continue // a bare repository: no submodules
		}
		gitDir := filepath.Join(rp.dir, ".git")
		if st, err := os.Lstat(gitDir); err != nil {
			return nil, c.unreadable(gitDir, err)
		} else if !st.IsDir() {
			continue // a submodule's .git file: its directory is in the parent's modules
		}
		if err := c.modules(filepath.Join(gitDir, "modules"), &modules); err != nil {
			return nil, err
		}
	}
	return append(repos, modules...), nil
}

// modules adds each git directory under dir, and under its own modules.
func (c *checker) modules(dir string, out *[]repo) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return c.unreadable(dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isGitDir(p) {
			rel, _ := filepath.Rel(c.top, p)
			*out = append(*out, repo{dir: p, gitDir: true, rel: rel, kind: "submodule without checkout"})
			if err := c.modules(filepath.Join(p, "modules"), out); err != nil {
				return err
			}
			continue
		}
		if err := c.modules(p, out); err != nil { // a module name with a slash
			return err
		}
	}
	return nil
}

func isGitDir(p string) bool {
	head, err1 := os.Lstat(filepath.Join(p, "HEAD"))
	objects, err2 := os.Lstat(filepath.Join(p, "objects"))
	return err1 == nil && err2 == nil && head.Mode().IsRegular() && objects.IsDir()
}

func (c *checker) unreadable(p string, err error) error {
	rel, rerr := filepath.Rel(c.top, p)
	if rerr != nil {
		rel = p
	}
	return &Refusal{Reason: Unreadable, Detail: rel + ": " + err.Error()}
}

// open makes a read-only reader for rp and checks that git sees the
// repository there, and only there.
func (c *checker) open(rp repo) (*reader, error) {
	g := &reader{dir: rp.dir, env: append(c.r.Maker.gitEnv(),
		"GIT_OPTIONAL_LOCKS=0",
		// Replacements and grafts could make a commit look like an
		// ancestor of a remote's.
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_GRAFT_FILE="+os.DevNull,
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(rp.dir),
		"LC_ALL=C")}
	var got string
	var err error
	if rp.gitDir {
		got, err = g.line(c.ctx, "rev-parse", "--absolute-git-dir")
	} else {
		got, err = g.line(c.ctx, "rev-parse", "--show-toplevel")
	}
	if err != nil {
		return nil, err
	}
	if got, err = filepath.EvalSymlinks(got); err != nil {
		return nil, err
	}
	if got != rp.dir {
		return nil, c.refuse(rp, GitError, "git finds the repository at "+got)
	}
	if err := g.disarm(c.ctx); err != nil {
		return nil, err
	}
	return g, nil
}

// files is step 1 for one work tree: nothing modified, staged, untracked,
// hidden from status, or ignored and not disposable, and no linked
// worktree, whose files are elsewhere.
func (c *checker) files(g *reader, rp repo) error {
	out, _, err := g.run(c.ctx, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=no", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r == "" || r[0] == '#' {
			continue
		}
		var fields []string
		switch r[0] {
		case '1':
			fields = strings.SplitN(r, " ", 9)
		case '2':
			fields = strings.SplitN(r, " ", 10)
			i++ // the original path
		case 'u':
			fields = strings.SplitN(r, " ", 11)
		case '?':
			return c.refuse(rp, Untracked, r[2:])
		default:
			return fmt.Errorf("workspace: git status printed %q", r)
		}
		if len(fields) < 9 || len(fields[1]) != 2 {
			return fmt.Errorf("workspace: git status printed %q", r)
		}
		name := fields[len(fields)-1]
		switch {
		case r[0] == 'u' || fields[1][1] != '.':
			return c.refuse(rp, Modified, name)
		case fields[1][0] != '.':
			return c.refuse(rp, Staged, name)
		}
	}

	// assume-unchanged (a lower-case tag) and skip-worktree ("S") files are
	// left out of status, changed or not.
	out, _, err = g.run(c.ctx, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for _, r := range strings.Split(string(out), "\x00") {
		if len(r) < 3 {
			continue
		}
		if tag := rune(r[0]); unicode.IsLower(tag) || tag == 'S' {
			return c.refuse(rp, Hidden, r[2:])
		}
	}

	out, _, err = g.run(c.ctx, "ls-files", "-z", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return err
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" && !c.disposable(name) {
			return c.refuse(rp, Ignored, name)
		}
	}

	worktrees, err := g.gitPath(c.ctx, "worktrees")
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(worktrees); err == nil && len(entries) > 0 {
		return c.refuse(rp, Worktree, entries[0].Name())
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c.unreadable(worktrees, err)
	}
	return nil
}

// disposable reports whether an ignored path, relative to its repository
// and with "/" after a directory, matches a disposable pattern.
func (c *checker) disposable(name string) bool {
	segs := strings.Split(strings.TrimSuffix(name, "/"), "/")
	for _, pat := range c.r.Disposable {
		pat = strings.Trim(pat, "/")
		if pat == "" {
			continue
		}
		for i := range segs {
			subject := segs[i]
			if strings.Contains(pat, "/") {
				subject = strings.Join(segs[:i+1], "/")
			}
			if ok, _ := path.Match(pat, subject); ok {
				return true
			}
		}
	}
	return false
}

// commits is steps 2 and 3 for one repository: every commit reachable from
// a ref, HEAD, the stash or a reflog is on a remote that counts, asked now.
func (c *checker) commits(g *reader, rp repo) error {
	out, _, err := g.run(c.ctx, "remote")
	if err != nil {
		return err
	}
	var urls []string
	var skipped *Refusal
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, "-") {
			return c.refuse(rp, GitError, "remote name "+strconv.Quote(name))
		}
		u, err := g.line(c.ctx, "ls-remote", "--get-url", name)
		if err != nil {
			return err
		}
		addr, reason := c.localRemote(u, rp.dir)
		if reason != "" {
			if skipped == nil {
				skipped = &Refusal{Reason: reason, Repo: rp.rel, Kind: rp.kind, Detail: "remote " + name + " is " + redact(u)}
			}
			continue
		}
		urls = append(urls, addr)
	}

	tips, scratch, rewritten, err := c.ask(g, rp, urls)
	if scratch != "" {
		defer os.RemoveAll(scratch)
	}
	if err != nil {
		return err
	}
	if skipped == nil {
		skipped = rewritten
	}
	// The remotes' objects are read from the scratch repository; nothing is
	// written into this one.
	local := *g
	local.env = append(append([]string(nil), g.env...), "GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(scratch, "objects"))
	unpushed := func(revs ...string) (string, error) {
		return local.input(c.ctx, tips, append([]string{"rev-list", "-n1"}, append(revs, "--stdin")...)...)
	}
	// HEAD and the other pseudo-refs are named: without a reflog, a
	// detached HEAD or ORIG_HEAD is on no ref.
	pseudo, err := c.pseudoRefs(g)
	if err != nil {
		return err
	}
	first, err := unpushed(append([]string{"--all", "--reflog"}, pseudo...)...)
	if err != nil || first == "" {
		return err
	}
	if skipped != nil {
		skipped.Detail += "; commit " + first + " is on no other remote"
		return skipped
	}

	// Name the reason by the first place that holds an unpushed commit.
	stash, err := c.stashCommits(g)
	if err != nil {
		return err
	}
	places := []struct {
		reason Reason
		revs   []string
	}{
		{UnpushedRef, []string{"--branches"}},
		{DetachedHEAD, nil},
		{Stash, stash},
		{RemoteGone, []string{"--remotes"}},
		{OtherRef, append([]string{"--all"}, pseudo...)},
	}
	if len(pseudo) > 0 && pseudo[0] == "HEAD" {
		places[1].revs = []string{"HEAD"}
	}
	for _, p := range places {
		if len(p.revs) == 0 {
			continue
		}
		commit, err := unpushed(p.revs...)
		if err != nil {
			return err
		}
		if commit != "" {
			return c.refuse(rp, p.reason, "commit "+commit)
		}
	}
	return c.refuse(rp, ReflogOnly, "commit "+first)
}

// pseudoRefs are those of HEAD, ORIG_HEAD, MERGE_HEAD, CHERRY_PICK_HEAD,
// REVERT_HEAD and REBASE_HEAD that name a commit, in that order.
func (c *checker) pseudoRefs(g *reader) ([]string, error) {
	var refs []string
	for _, ref := range []string{"HEAD", "ORIG_HEAD", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD"} {
		_, code, err := g.run(c.ctx, "rev-parse", "--quiet", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil && code != 1 { // 1: absent, or no commit yet
			return nil, err
		}
		if err == nil {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// stashCommits are the commits of every stash entry.
func (c *checker) stashCommits(g *reader) ([]string, error) {
	if _, code, err := g.run(c.ctx, "rev-parse", "--quiet", "--verify", "refs/stash"); code == 1 {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	out, _, err := g.run(c.ctx, "log", "--walk-reflogs", "--format=%H", "refs/stash", "--")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// ask fetches every ref of each remote into a scratch repository of its
// own, under the instance's state, that borrows the repository's objects
// so that only what it lacks is downloaded. It returns one "^<commit>"
// line per commit a remote ref names, and the scratch repository, which
// the caller removes.
func (c *checker) ask(g *reader, rp repo, urls []string) (tips, scratch string, rewritten *Refusal, err error) {
	objects, err := g.gitPath(c.ctx, "objects")
	if err != nil {
		return "", "", nil, err
	}
	if objects, err = filepath.Abs(objects); err != nil {
		return "", "", nil, err
	}
	if _, err := realDir(filepath.Join(c.root, "state"), true); err != nil {
		return "", "", nil, err
	}
	base, err := realDir(filepath.Join(c.root, scratchDir), true)
	if err != nil {
		return "", "", nil, err
	}
	var b [8]byte
	rand.Read(b[:])
	c.n++
	scratch = filepath.Join(base, hex.EncodeToString(b[:])+"-"+strconv.Itoa(c.n))
	s := reader{dir: scratch, env: append(c.r.Maker.gitEnv(),
		"GIT_ALLOW_PROTOCOL=https:http:ssh:git:file",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_GRAFT_FILE="+os.DevNull,
		"GIT_CEILING_DIRECTORIES="+base,
		"LC_ALL=C")}
	if _, _, err := (reader{dir: base, env: s.env}).run(c.ctx, "init", "--quiet", "--bare", "--", scratch); err != nil {
		return "", scratch, nil, err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return "", scratch, nil, err
	}
	for i, u := range urls {
		// git rewrites the address again by url.<base>.insteadOf: what
		// was checked must be what is fetched.
		// One that would be rewritten does not count.
		if got, err := s.line(c.ctx, "ls-remote", "--get-url", "--", u); err != nil {
			return "", scratch, nil, err
		} else if got != u {
			if rewritten == nil {
				rewritten = &Refusal{Reason: RemoteUnresolved, Repo: rp.rel, Kind: rp.kind, Detail: "remote " + redact(u) + " is rewritten to " + redact(got)}
			}
			continue
		}
		if _, _, err := s.run(c.ctx, "-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
			"--", u, fmt.Sprintf("+refs/*:refs/r%d/*", i)); err != nil {
			return "", scratch, nil, err
		}
	}
	out, _, err := s.run(c.ctx, "for-each-ref", "--format=%(objectname) %(objecttype) %(*objectname) %(*objecttype)")
	if err != nil {
		return "", scratch, nil, err
	}
	var list strings.Builder
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) >= 2 && f[1] == "commit":
			fmt.Fprintf(&list, "^%s\n", f[0])
		case len(f) == 4 && f[3] == "commit":
			fmt.Fprintf(&list, "^%s\n", f[2])
		}
	}
	return list.String(), scratch, rewritten, nil
}

// localRemote says whether a remote address counts, and returns the
// address to fetch it by. A local one does not count when git would read a
// repository in the instance's workspaces, or under this or another hataori
// root, or when it cannot be resolved the way git resolves it.
func (c *checker) localRemote(u, dir string) (string, Reason) {
	p, local := c.localPath(u, dir)
	switch {
	case !local:
		return u, ""
	case p == "":
		return u, RemoteUnresolved
	}
	reads, err := gitReads(p)
	if err != nil {
		return u, RemoteUnresolved
	}
	for _, r := range reads {
		if reason := c.inRoot(r); reason != "" {
			return u, reason
		}
	}
	// git serves the objects of the remote's alternates as its own.
	if reason, err := c.alternates(reads); err != nil {
		return u, RemoteUnresolved
	} else if reason != "" {
		return u, reason
	}
	if strings.HasPrefix(u, "file://") {
		return u, ""
	}
	// The scratch repository fetches from elsewhere: a relative or ~/
	// path is given as the path git would read in the repository.
	return p, ""
}

// alternateDepth is how deep git follows objects/info/alternates: the
// remote's own file and five more levels.
const alternateDepth = 5

// objectEntryLimit bounds how many entries one check lists below a
// remote's objects directories.
const objectEntryLimit = 1 << 20

// alternates names why the objects git serves for a remote whose paths are
// reads come from inside a root, or "". For each one it walks the objects
// directory and the objects/info/alternates git follows from there, in
// git's order. A path counts as inside a root when it passes through one
// on the way, not only when it ends there: a link through a workspace
// breaks when the workspace is moved. An alternate that cannot be
// resolved or is not a directory, or a file nested deeper than git reads,
// is an error: what git serves then cannot be told.
func (c *checker) alternates(reads []string) (Reason, error) {
	budget := objectEntryLimit
	if c.r.objectLimit != 0 {
		budget = c.r.objectLimit
	}
	for _, r := range reads {
		objects, reason, err := c.walk(r + "/objects")
		if missing(err) {
			continue
		} else if err != nil || reason != "" {
			return reason, err
		}
		if reason, err := c.objectLinks(objects, &budget); err != nil || reason != "" {
			return reason, err
		}
		seen := map[string]bool{objects: true}
		if reason, err := c.readAlternates(objects, 0, seen, &budget); err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

// readAlternates follows objects/info/alternates of the objects directory
// objects at depth as git does: depth first, each entry linked and its own
// alternates read before the next, a directory seen before skipped.
func (c *checker) readAlternates(objects string, depth int, seen map[string]bool, budget *int) (Reason, error) {
	file, reason, err := c.walk(objects + "/info/alternates")
	if missing(err) {
		return "", nil
	} else if err != nil || reason != "" {
		return reason, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", nil
	}
	if depth > alternateDepth {
		return "", errors.New("alternates nested deeper than git reads them: " + objects)
	}
	for _, e := range parseAlternates(string(b)) {
		// A relative path is relative to the resolved objects directory.
		if !filepath.IsAbs(e) {
			e = objects + "/" + e
		}
		alt, reason, err := c.walk(e)
		if err != nil || reason != "" {
			return reason, err
		}
		if fi, err := os.Stat(alt); err != nil {
			return "", err
		} else if !fi.IsDir() {
			return "", errors.New("alternate " + alt + " is not a directory")
		}
		if seen[alt] {
			continue
		}
		seen[alt] = true
		if reason, err := c.objectLinks(alt, budget); err != nil || reason != "" {
			return reason, err
		}
		if reason, err := c.readAlternates(alt, depth+1, seen, budget); err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

// objectLinks names why a file git may read as an object below the
// resolved objects directory objects comes from inside a root, or "".
// git reads every file in pack/ (and, through a multi-pack-index, possibly
// below it) and in the lower-case fan-out directories, through links;
// info/ holds the alternates and the commit-graph chain. Each of those
// directories that is a link, and each link in them, is walked as walk
// does; pack/ and info/ are looked at all the way down, the fan-out
// directories only one level and only names a loose object can have, as
// git reads them. Nothing else is
// followed. Listing more than budget entries in all is an error: what git
// reads then is not told.
func (c *checker) objectLinks(objects string, budget *int) (Reason, error) {
	seen := map[string]int{}
	return c.eachEntry(objects, budget, func(e fs.DirEntry) (Reason, error) {
		name := e.Name()
		if name != "pack" && name != "info" && !fanOut(name) {
			return "", nil // git reads nothing there
		}
		return c.linksBelow(filepath.Join(objects, name), e, budget, !fanOut(name), seen)
	})
}

// linksBelow walks p, the entry e of a directory, when it is a link, and
// then the links in what it is or leads to, when that is a directory:
// below it as well when deep. A directory is looked at once at each
// depth: one first reached from a fan-out directory is looked at again
// when pack/ or info/ reaches it.
func (c *checker) linksBelow(p string, e fs.DirEntry, budget *int, deep bool, seen map[string]int) (Reason, error) {
	dir := p
	if e.Type()&fs.ModeSymlink != 0 {
		resolved, reason, err := c.walk(p)
		if err != nil || reason != "" {
			return reason, err
		}
		dir = resolved
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	level := 1
	if deep {
		level = 2
	}
	if !fi.IsDir() || seen[dir] >= level {
		return "", nil
	}
	seen[dir] = level
	return c.eachEntry(dir, budget, func(f fs.DirEntry) (Reason, error) {
		q := filepath.Join(dir, f.Name())
		switch {
		case deep && (f.IsDir() || f.Type()&fs.ModeSymlink != 0):
			return c.linksBelow(q, f, budget, true, seen)
		case f.Type()&fs.ModeSymlink != 0 && lowerHex(f.Name()):
			_, reason, err := c.walk(q) // a loose object
			return reason, err
		}
		return "", nil
	})
}

// eachEntry calls fn with each entry of dir until it names a reason or
// fails, reading the directory in batches and counting each entry against
// budget.
func (c *checker) eachEntry(dir string, budget *int, fn func(fs.DirEntry) (Reason, error)) (Reason, error) {
	f, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for {
		batch, err := f.ReadDir(1024)
		if *budget -= len(batch); *budget < 0 {
			return "", errors.New("too many entries below the objects directories")
		}
		for _, e := range batch {
			if reason, err := fn(e); err != nil || reason != "" {
				return reason, err
			}
		}
		if errors.Is(err, io.EOF) {
			return "", nil
		} else if err != nil {
			return "", err
		}
		if err := c.ctx.Err(); err != nil {
			return "", err
		}
	}
}

// fanOut reports a name git reads loose objects from: two lower-case hex
// digits.
func fanOut(name string) bool {
	return len(name) == 2 && lowerHex(name)
}

// lowerHex reports a name of lower-case hex digits only, as git names
// fan-out directories and loose objects.
func lowerHex(name string) bool {
	return name != "" && strings.IndexFunc(name, func(r rune) bool { return !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') }) < 0
}

// walk resolves the absolute path p one component at a time, as the
// kernel does: a link is followed where it stands, ".." goes up from what
// is resolved so far, and a component after a file is ENOTDIR. It names
// the reason when any directory or link on the way is inside a root, the
// workspaces before any other: such a path does not count even when it
// cannot be resolved to the end.
func (c *checker) walk(p string) (string, Reason, error) {
	var hit Reason
	resolved, err := c.walkFrom(p, func(next string) {
		if reason := c.inRoot(next); reason == RemoteIsLocal || reason != "" && hit == "" {
			hit = reason
		}
	})
	if hit != "" {
		return "", hit, nil
	}
	return resolved, "", err
}

// walkFrom resolves p and calls on with every directory or link it
// passes, resolved up to it.
func (c *checker) walkFrom(p string, on func(next string)) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("relative path " + p)
	}
	cur, rest, links := "/", p, 0
	for {
		rest = strings.TrimLeft(rest, "/")
		if rest == "" {
			return cur, nil
		}
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		switch name {
		case ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		on(next)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if links++; links > 40 {
				return "", errors.New("too many links in " + p)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				cur = "/"
			}
			rest = target + "/" + rest
			continue
		}
		if !fi.IsDir() && strings.Trim(rest, "/") != "" {
			return "", &fs.PathError{Op: "walk", Path: next, Err: syscall.ENOTDIR}
		}
		cur = next
	}
}

// parseAlternates splits an objects/info/alternates file as git does: one
// path per line, "#" starts a comment line, and a line starting with a
// double quote is a C-quoted path, after which one more byte is skipped. A
// quoted path git cannot unquote is taken as it is, quotes and all.
func parseAlternates(s string) []string {
	var entries []string
	for len(s) > 0 {
		var entry string
		if s[0] == '"' {
			if v, n, ok := unquoteC(s); ok {
				entry, s = v, s[n:]
				if len(s) > 0 {
					s = s[1:]
				}
				if entry != "" {
					entries = append(entries, entry)
				}
				continue
			}
		}
		line, rest, _ := strings.Cut(s, "\n")
		if line != "" && line[0] != '#' {
			entry = line
			entries = append(entries, entry)
		}
		s = rest
	}
	return entries
}

// unquoteC unquotes a C-quoted string at the start of s the way git does,
// returning it and the number of bytes it took, closing quote included.
func unquoteC(s string) (string, int, bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '"':
			return b.String(), i + 1, true
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, false
			}
			switch e := s[i]; e {
			case '"', '\\':
				b.WriteByte(e)
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'v':
				b.WriteByte('\v')
			case '0', '1', '2', '3':
				if i+2 >= len(s) || s[i+1] < '0' || s[i+1] > '7' || s[i+2] < '0' || s[i+2] > '7' {
					return "", 0, false
				}
				b.WriteByte((e-'0')<<6 | (s[i+1]-'0')<<3 | (s[i+2] - '0'))
				i += 2
			default:
				return "", 0, false
			}
		default:
			b.WriteByte(ch)
		}
	}
	return "", 0, false
}

// missing reports an error that says a path is not there.
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// inRoot names why a resolved path does not count, or "".
func (c *checker) inRoot(p string) Reason {
	if p == filepath.Join(c.root, Dir) || within(p, filepath.Join(c.root, Dir)) {
		return RemoteIsLocal
	}
	roots := append([]string{c.root}, c.r.OtherRoots...)
	for _, root := range roots {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		if root = filepath.Clean(root); p == root || within(p, root) {
			return RemoteInRoot
		}
	}
	return ""
}

// localPath is the path a remote address names on this machine, the way
// git reads it: a file:// URL, or a path with no scheme and no host:
// before its first slash. A relative path is joined to dir, and a leading
// ~/ to $HOME, as text: nothing is cleaned, because ".." after a symlink
// goes up from the link's target. local is false for a remote address,
// and the path is "" for a local one that cannot be told.
func (c *checker) localPath(u, dir string) (p string, local bool) {
	switch {
	case strings.HasPrefix(u, "file://"):
		// git decodes %XX in the whole URL, then takes the host up to
		// the first slash (after a bracketed host's "]").
		p, err := url.PathUnescape(strings.TrimPrefix(u, "file://"))
		if err != nil {
			return "", true
		}
		from := 0
		if strings.HasPrefix(p, "[") {
			if end := strings.IndexByte(p, ']'); end > 0 {
				from = end
			}
		}
		i := strings.IndexByte(p[from:], '/')
		if i < 0 {
			return "", true
		}
		return p[from+i:], true
	case strings.Contains(u, "://"):
		return "", false
	}
	colon, slash := strings.IndexByte(u, ':'), strings.IndexByte(u, '/')
	if colon >= 0 && (slash < 0 || colon < slash) {
		return "", false // [user@]host:path
	}
	switch {
	case u == "~" || strings.HasPrefix(u, "~/"):
		// upload-pack expands it with the HOME git runs with.
		home := ""
		for _, kv := range c.r.Maker.gitEnv() {
			if v, ok := strings.CutPrefix(kv, "HOME="); ok {
				home = v
			}
		}
		if !filepath.IsAbs(home) {
			return "", true
		}
		return home + "/" + strings.TrimPrefix(u[1:], "/"), true
	case strings.HasPrefix(u, "~"): // ~user
		return "", true
	case filepath.IsAbs(u):
		return u, true
	case !filepath.IsAbs(dir):
		return "", true
	}
	return dir + "/" + u, true
}

// gitReads lists, resolved, every path git may read the repository at p
// from: p itself (a git directory, a .git file or a bundle), and p/.git,
// p.git/.git and p.git, which upload-pack also tries, and the git
// directory a .git file among them names. Trailing slashes are dropped
// first, as upload-pack drops them. A path that does not exist is left
// out, as git leaves it out; one that cannot be resolved is an error, and
// so is finding nothing: git may still read something there.
func gitReads(p string) ([]string, error) {
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	var reads []string
	resolve := func(q string) (string, bool, error) {
		r, err := filepath.EvalSymlinks(q)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", false, nil
		}
		return r, err == nil, err
	}
	for _, suffix := range []string{"", "/.git", ".git/.git", ".git"} {
		q := p + suffix
		r, ok, err := resolve(q)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if reads, err = withCommonDir(reads, r); err != nil {
			return nil, err
		}
		gitdir, err := readGitfile(r)
		if err != nil {
			return nil, err
		}
		if gitdir == "" {
			continue
		}
		// git reads a relative gitdir from the directory the path names,
		// before it is resolved.
		if !filepath.IsAbs(gitdir) {
			gitdir = q[:strings.LastIndexByte(q, '/')+1] + gitdir
		}
		// A .git file naming nothing that exists is not followed by git
		// either, but it cannot be told what git would read instead.
		if r, ok, err = resolve(gitdir); err != nil {
			return nil, err
		} else if !ok {
			return nil, errors.New(".git file " + q + " names nothing")
		}
		if reads, err = withCommonDir(reads, r); err != nil {
			return nil, err
		}
	}
	if len(reads) == 0 {
		return nil, errors.New("nothing at " + p)
	}
	return reads, nil
}

// withCommonDir appends the resolved git directory r and, when r is a
// linked worktree's git directory, the common directory its commondir
// file names, where git reads the refs and objects.
func withCommonDir(reads []string, r string) ([]string, error) {
	reads = append(reads, r)
	b, err := os.ReadFile(filepath.Join(r, "commondir"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return reads, nil
	} else if err != nil {
		return nil, err
	}
	common := strings.TrimRight(string(b), "\r\n")
	if !filepath.IsAbs(common) {
		common = r + "/" + common
	}
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		return nil, err // git refuses a missing one too
	}
	return append(reads, resolved), nil
}

// readGitfile returns the git directory a .git file names, or "" for a
// directory or any other file.
func readGitfile(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<16+1))
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(b, []byte("gitdir: ")) {
		return "", nil
	}
	if len(b) > 1<<16 {
		return "", errors.New("long .git file " + p)
	}
	gitdir := strings.TrimRight(string(b[len("gitdir: "):]), "\r\n")
	if gitdir == "" {
		return "", errors.New(".git file " + p + " names nothing")
	}
	return gitdir, nil
}

// processes is step 4: no process has a file or its working directory in
// the workspace. lsof is asked on Linux and on macOS alike; a missing or
// failing lsof refuses. Only the processes of the user hataori runs as can
// be seen, unless it runs as root.
func (c *checker) processes() error {
	args := []string{"-n", "-P", "-w", "-F0pn"}
	if uid := os.Geteuid(); uid != 0 {
		args = append(args, "-u", strconv.Itoa(uid))
	}
	cmd := exec.CommandContext(c.ctx, "lsof", args...)
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return &Refusal{Reason: UnknownUse, Detail: "lsof: " + lastLines(stderr.String(), 3, err.Error())}
	}
	pid := ""
	for _, f := range strings.Split(stdout.String(), "\x00") {
		f = strings.TrimLeft(f, "\n")
		if f == "" {
			continue
		}
		switch f[0] {
		case 'p':
			pid = f[1:]
		case 'n':
			name := f[1:]
			if name == c.top || strings.HasPrefix(name, c.top+"/") || strings.HasPrefix(name, c.top+" ") {
				return &Refusal{Reason: InUse, Detail: "process " + pid + " uses " + name}
			}
		}
	}
	return nil
}

// input runs git with stdin and returns its output's first line, or "".
func (g reader) input(ctx context.Context, stdin string, args ...string) (string, error) {
	full := append(append([]string{"-C", g.dir, "-c", "core.fsmonitor=false"}, g.extra...), args...)
	cmd := gitCommand(ctx, full...)
	cmd.Env = g.env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("workspace: git %s: %s", args[0], lastLines(redact(stderr.String()), 3, redactErr(err)))
	}
	first, _, _ := strings.Cut(strings.TrimSpace(stdout.String()), "\n")
	return first, nil
}
