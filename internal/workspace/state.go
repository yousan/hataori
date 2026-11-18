package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// State reads what the workspace at dir holds: its branch, the commit the
// branch started from, the lines changed against it, the uncommitted files,
// the commits on no remote-tracking ref, and when the remote-tracking refs
// were last updated. It implements the core's Workspaces.
//
// It changes nothing in dir: git runs with optional locks off, so not even
// the index's cached file times are written, and it never asks a remote.
// A directory that is not the top of a repository, and any git failure,
// is an error: the numbers are never guessed.
func (m Maker) State(ctx context.Context, dir string) (control.WorkspaceState, error) {
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
		// git looks for the repository in dir only, never in a directory above.
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(abs),
		"LC_ALL=C")}

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

	if st.Base, err = g.base(ctx, head); err != nil {
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
	dir string
	env []string
}

// run runs git and returns its output, and its exit code when it ran and
// failed.
func (g reader) run(ctx context.Context, args ...string) ([]byte, int, error) {
	full := append([]string{"-C", g.dir, "-c", "core.fsmonitor=false"}, args...)
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
	s := strings.TrimSpace(string(out))
	if s == "" || strings.Contains(s, "\n") {
		return "", fmt.Errorf("workspace: git %s printed %q", args[0], s)
	}
	return s, nil
}

// base is the commit the branch started from: where HEAD meets the
// remote's default branch, or else the branch's upstream. With neither,
// nothing is known to have come from elsewhere, and it is HEAD.
func (g reader) base(ctx context.Context, head string) (string, error) {
	for _, ref := range []string{"refs/remotes/origin/HEAD", "@{upstream}"} {
		// Exit 1 with --quiet: the ref does not exist.
		target, code, err := g.run(ctx, "rev-parse", "--quiet", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil && code == 1 {
			continue
		}
		if err != nil && ref == "@{upstream}" && code == 128 {
			// No upstream is configured, or HEAD is detached.
			continue
		}
		if err != nil {
			return "", err
		}
		return g.line(ctx, "merge-base", "--end-of-options", head, strings.TrimSpace(string(target)))
	}
	return head, nil
}

// fetched is the latest of the last fetch (FETCH_HEAD) and the newest entry
// of any remote-tracking ref's reflog, which the clone writes. nil when
// there is neither.
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
