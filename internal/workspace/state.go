package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// State reads what the workspace at dir holds: its branch, the commit the
// branch started from, the lines changed against it, the uncommitted files,
// the commits on no remote-tracking ref, and when the remote-tracking refs
// were last updated. It implements the core's Workspaces.
//
// It changes nothing in dir: git runs with optional locks off, so not even
// the index's cached file times are written, no filter driver runs, and it
// never asks a remote: a partial clone is refused.
// A directory that is not the top of a repository, and any git failure,
// is an error: the numbers are never guessed.
func (m Maker) State(ctx context.Context, dir string) (control.WorkspaceState, error) {
	st, err := m.state(ctx, dir)
	if err != nil {
		return control.WorkspaceState{}, err // nothing half read
	}
	return st, nil
}

func (m Maker) state(ctx context.Context, dir string) (control.WorkspaceState, error) {
	var st control.WorkspaceState
	abs, err := filepath.Abs(dir)
	if err != nil {
		return st, fmt.Errorf("workspace: %w", err)
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return st, fmt.Errorf("workspace: %w", err)
	}
	g := reader{dir: abs, env: append(m.gitEnv(),
		"GIT_OPTIONAL_LOCKS=0",
		// A partial clone would fetch missing objects (refused below; this
		// is for a git that knows the variable).
		"GIT_NO_LAZY_FETCH=1",
		// git looks for the repository in dir only, never in a directory above.
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(abs),
		"LC_ALL=C")}
	// diff refreshes the index (and runs post-index-change) even with
	// optional locks off when only a tracked file's cached times differ.
	g.set("diff.autoRefreshIndex", "false")

	top, err := g.line(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return st, err
	}
	if top, err = filepath.EvalSymlinks(top); err != nil {
		return st, fmt.Errorf("workspace: %w", err)
	}
	if top != abs {
		return st, fmt.Errorf("workspace: %s is not the top of a repository", dir)
	}
	if err := g.disarm(ctx); err != nil {
		return st, err
	}
	head, err := g.line(ctx, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return st, err
	}

	// Detached: symbolic-ref exits 1 and prints nothing.
	if out, code, err := g.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil && code != 1 {
		return st, err
	} else if err == nil {
		st.Branch = strings.TrimSpace(string(out))
	}

	if st.Base, err = g.base(ctx, head, st.Branch); err != nil {
		return st, err
	}
	out, _, err := g.run(ctx, "diff", "--numstat", "-z", "-M", "--no-ext-diff", "--no-textconv", "--end-of-options", st.Base, "--")
	if err != nil {
		return st, err
	}
	if st.Added, st.Removed, err = numstat(out); err != nil {
		return st, err
	}
	out, _, err = g.run(ctx, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return st, err
	}
	if st.Uncommitted, err = statusFiles(out); err != nil {
		return st, err
	}
	n, err := g.line(ctx, "rev-list", "--count", "HEAD", "--branches", "--not", "--remotes")
	if err != nil {
		return st, err
	}
	if st.Unpushed, err = strconv.Atoi(n); err != nil {
		return st, fmt.Errorf("workspace: git rev-list --count printed %q", n)
	}
	if st.Fetched, err = g.fetched(ctx); err != nil {
		return st, err
	}
	return st, nil
}

// reader runs git in one repository, read-only.
type reader struct {
	dir   string
	env   []string
	extra []string // options for every git command after the first
}

// disarm reads the repository's configuration and refuses what would make
// git write or ask a remote: a partial clone, which fetches missing objects
// on demand. It turns off every filter driver, whose clean command diff and
// status would run on changed files and which can do anything: the ones that
// the submodules below configure too, as git runs git status there and hands
// it these options.
func (g *reader) disarm(ctx context.Context) error {
	// Exit 1: none is set.
	out, code, err := g.run(ctx, "config", "--null", "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor)$`)
	if err != nil && code != 1 {
		return err
	}
	for _, kv := range strings.Split(string(out), "\x00") {
		key, value, _ := strings.Cut(kv, "\n")
		if key == "extensions.partialclone" || strings.HasSuffix(key, ".promisor") && isTrue(value) {
			return fmt.Errorf("workspace: %s is a partial clone, which git may complete from the remote", g.dir)
		}
	}
	own, err := g.filters(ctx)
	if err != nil {
		return err
	}
	g.turnOff(own)
	names := map[string]bool{}
	if err := g.submoduleFilters(ctx, names, map[string]bool{g.dir: true}); err != nil {
		return err
	}
	for _, name := range own {
		delete(names, name)
	}
	below := slices.Collect(maps.Keys(names))
	g.turnOff(below)
	for _, name := range below {
		// git would fail on a changed file under a filter that is required.
		g.set("filter."+name+".required", "false")
	}
	return nil
}

// turnOff makes every later git command, and the ones git runs in
// submodules, run no filter driver of these names.
func (g *reader) turnOff(names []string) {
	for _, name := range names {
		g.set("filter."+name+".clean", "")
		g.set("filter."+name+".process", "")
	}
}

// set gives every later git command a configuration value.
func (g *reader) set(key, value string) {
	if strings.Contains(key, "=") { // -c would end the key there
		env := fmt.Sprintf("HATAORI_GIT_VALUE_%d", len(g.extra))
		g.env = append(slices.Clip(g.env), env+"="+value)
		g.extra = append(g.extra, "--config-env="+key+"="+env)
		return
	}
	g.extra = append(g.extra, "-c", key+"="+value)
}

// filters are the names of the filter drivers that the repository's
// configuration has a command for.
func (g reader) filters(ctx context.Context) ([]string, error) {
	// Exit 1: none is set.
	out, code, err := g.run(ctx, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	if err != nil && code != 1 {
		return nil, err
	}
	var names []string
	for _, key := range strings.Split(string(out), "\x00") {
		if key != "" {
			names = append(names, key[len("filter."):strings.LastIndex(key, ".")])
		}
	}
	return names, nil
}

// submoduleFilters adds the filters of the submodules below the repository
// to names. Directories in seen have been read.
func (g reader) submoduleFilters(ctx context.Context, names, seen map[string]bool) error {
	links, err := g.gitlinks(ctx)
	if err != nil {
		return err
	}
	for _, link := range links {
		// git 2.43 follows a symbolic link in the place of a submodule.
		dir := filepath.Join(g.dir, link)
		if _, err := os.Stat(filepath.Join(dir, ".git")); missing(err) || errors.Is(err, syscall.ELOOP) {
			continue // not checked out
		} else if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		// git runs git status there with this GIT_DIR.
		sub := reader{dir: dir, env: append(slices.Clip(g.env), "GIT_DIR=.git")}
		// Its own submodules are below its work tree, which core.worktree
		// can put elsewhere.
		at, _, err := sub.run(ctx, "rev-parse", "--is-inside-work-tree", "--show-prefix")
		if err != nil {
			return err
		}
		if string(at) != "true\n\n" {
			return fmt.Errorf("workspace: the work tree of the submodule at %s is elsewhere", dir)
		}
		filters, err := sub.filters(ctx)
		if err != nil {
			return err
		}
		for _, name := range filters {
			names[name] = true
		}
		if err := sub.submoduleFilters(ctx, names, seen); err != nil {
			return err
		}
	}
	return nil
}

// gitlinks are the paths of the submodules in the index.
func (g reader) gitlinks(ctx context.Context) ([]string, error) {
	out, _, err := g.run(ctx, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var links []string
	for _, r := range strings.Split(string(out), "\x00") {
		if r == "" {
			continue
		}
		info, name, ok := strings.Cut(r, "\t")
		if !ok {
			return nil, fmt.Errorf("workspace: git ls-files -s printed %q", r)
		}
		if strings.HasPrefix(info, "160000 ") {
			links = append(links, name)
		}
	}
	return links, nil
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "", "true", "yes", "on", "1": // a key with no value is true
		return true
	}
	return false
}

// run runs git and returns its output, and its exit code when it ran and
// failed.
func (g reader) run(ctx context.Context, args ...string) ([]byte, int, error) {
	full := append(append([]string{"-C", g.dir, "-c", "core.fsmonitor=false"}, g.extra...), args...)
	cmd := gitCommand(ctx, full...)
	cmd.Env = g.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return nil, code, fmt.Errorf("workspace: git %s: %s", args[0], lastLines(redact(stderr.String()), 3, redactErr(err)))
	}
	return stdout.Bytes(), 0, nil
}

// line runs git and returns its output's only line.
func (g reader) line(ctx context.Context, args ...string) (string, error) {
	out, _, err := g.run(ctx, args...)
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(string(out), "\n")
	if s == "" || strings.Contains(s, "\n") {
		return "", fmt.Errorf("workspace: git %s printed %q", args[0], s)
	}
	return s, nil
}

// base is the commit the branch started from: where HEAD meets the
// remote's default branch, or else the branch's upstream. With neither,
// nothing is known to have come from elsewhere, and it is HEAD. An upstream
// that is configured but cannot be read is an error.
func (g reader) base(ctx context.Context, head, branch string) (string, error) {
	// Exit 1 with --quiet: origin/HEAD does not exist.
	target, code, err := g.run(ctx, "rev-parse", "--quiet", "--verify", "--end-of-options", "refs/remotes/origin/HEAD^{commit}")
	if err != nil && code != 1 {
		return "", err
	}
	if err != nil && branch != "" {
		up, _, uerr := g.run(ctx, "for-each-ref", "--format=%(upstream)", "--end-of-options", "refs/heads/"+branch)
		if uerr != nil {
			return "", uerr
		}
		if u := strings.TrimSpace(string(up)); u != "" {
			t, err := g.line(ctx, "rev-parse", "--verify", "--end-of-options", u+"^{commit}")
			if err != nil {
				return "", err
			}
			target = []byte(t)
		}
	} else if err != nil {
		target = nil
	}
	if len(target) == 0 {
		return head, nil
	}
	return g.line(ctx, "merge-base", "--end-of-options", head, strings.TrimSpace(string(target)))
}

// fetched is the latest of the last fetch (FETCH_HEAD) and the newest entry
// of any remote-tracking ref's reflog, which the clone, a fetch and a push
// write. nil when there is neither.
func (g reader) fetched(ctx context.Context) (*time.Time, error) {
	var latest time.Time
	fetchHead, err := g.gitPath(ctx, "FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(fetchHead); err == nil {
		latest = fi.ModTime()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	logs, err := g.gitPath(ctx, "logs/refs/remotes")
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(logs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == logs && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		t, err := lastReflogTime(p)
		if err != nil {
			return err
		}
		if t.After(latest) {
			latest = t
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: reflog: %w", err)
	}
	if latest.IsZero() {
		return nil, nil
	}
	latest = latest.UTC()
	return &latest, nil
}

func (g reader) gitPath(ctx context.Context, name string) (string, error) {
	p, err := g.line(ctx, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.dir, p)
	}
	return p, nil
}

// lastReflogTime is the time of a reflog file's last entry:
// "<old> <new> <name> <email> <seconds> <zone>\t<message>". An empty file
// has no time.
func lastReflogTime(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if l := sc.Text(); l != "" {
			last = l
		}
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, err
	}
	if last == "" {
		return time.Time{}, nil
	}
	head, _, _ := strings.Cut(last, "\t")
	f2 := strings.Fields(head)
	if len(f2) < 2 {
		return time.Time{}, fmt.Errorf("%s: unreadable entry", path)
	}
	sec, err := strconv.ParseInt(f2[len(f2)-2], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: unreadable entry", path)
	}
	return time.Unix(sec, 0), nil
}

// numstat sums the lines of `git diff --numstat -z`. A record is
// "<added>\t<removed>\t<path>" or, for a rename or copy,
// "<added>\t<removed>\t" followed by the two paths as records of their own.
// A binary file has "-" for both and adds no lines.
func numstat(out []byte) (added, removed int, err error) {
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r == "" {
			continue
		}
		a, rest, ok1 := strings.Cut(r, "\t")
		d, path, ok2 := strings.Cut(rest, "\t")
		if !ok1 || !ok2 {
			return 0, 0, fmt.Errorf("workspace: git diff --numstat printed %q", r)
		}
		if path == "" {
			i += 2 // the paths of a rename
		}
		if a == "-" && d == "-" {
			continue
		}
		na, err1 := strconv.Atoi(a)
		nd, err2 := strconv.Atoi(d)
		if err1 != nil || err2 != nil {
			return 0, 0, fmt.Errorf("workspace: git diff --numstat printed %q", r)
		}
		added += na
		removed += nd
	}
	return added, removed, nil
}

// statusFiles counts the files of `git status --porcelain=v2 -z`: changed
// ("1"), renamed or copied ("2", followed by the original path as a record
// of its own), unmerged ("u") and untracked ("?").
func statusFiles(out []byte) (int, error) {
	n := 0
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r == "" {
			continue
		}
		switch r[0] {
		case '1', 'u', '?':
			n++
		case '2':
			n++
			i++
		case '#', '!':
		default:
			return 0, fmt.Errorf("workspace: git status printed %q", r)
		}
	}
	return n, nil
}
