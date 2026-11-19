// Package workspace makes the working directory of a thread: a clone of one
// repository, at a path of its own that nothing else uses.
//
// This package never resets a workspace or a mirror in place. A workspace
// is removed only by Remover, which first proves that nothing in it exists
// nowhere else (design, section 4.6) and then renames it into trash/.
// Besides trash entries past their retention, it removes only temporary
// directories it created in the same call.
package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// Dir is the workspaces directory, relative to the instance root.
const Dir = "workspaces"

// cloneDir is where clones are made before they are complete, relative to
// the instance root: inside it, but outside Dir.
const cloneDir = "state/clones"

// ErrExists is returned when the workspace's directory already exists.
var ErrExists = errors.New("workspace: the directory exists")

// Maker makes workspaces under an instance root. It implements the core's
// Workspaces.
type Maker struct {
	Root string
	// FetchTimeout bounds mirror work, including waiting for its lock; zero means two minutes.
	FetchTimeout time.Duration
	// Environ is the daemon's environment; os.Environ when nil.
	Environ func() []string
}

// Path returns where a new workspace of thread is made:
// <root>/workspaces/<number>. A thread made before thread numbers has its
// workspace where it was made, <root>/workspaces/<client>/<scope>/<key>;
// a reader takes the path stored with the thread, never this one.
func Path(root string, thread control.ThreadID) string {
	return filepath.Join(root, Dir, strconv.FormatInt(int64(thread), 10))
}

// maxSegment bounds a path segment; a longer one is shortened and given a
// hash of the whole.
const maxSegment = 80

// Segment makes s safe as one path segment, so that different strings
// never share a segment: bytes other than letters, digits, '.', '_' and
// '-' become %XX; "." and ".." are escaped; the empty string is "%"; a
// segment longer than 80 bytes keeps its first 40 and adds a hash.
func Segment(s string) string {
	if s == "" {
		return "%"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	out := b.String()
	if strings.Trim(out, ".") == "" { // ".", "..", "..."
		out = strings.ReplaceAll(out, ".", "%2E")
	}
	if len(out) > maxSegment {
		sum := sha256.Sum256([]byte(s))
		out = out[:40] + "%" + hex.EncodeToString(sum[:12])
	}
	return out
}

// scpLike is git's SSH form [user@]host:path. The host has no '/', so
// "host/owner/repo" and "git@host/owner/repo" are not it.
var scpLike = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+:[^\s]+$`)

// CheckAddress accepts a URL with a scheme and a host or path, or the SSH
// form [user@]host:path. It looks at the string only.
func CheckAddress(repo string) error {
	if strings.TrimSpace(repo) != repo || repo == "" || strings.ContainsAny(repo, " \t\n\r") {
		return fmt.Errorf("workspace: repository address %q is empty or has spaces", redact(repo))
	}
	if strings.HasPrefix(repo, "-") {
		return fmt.Errorf("workspace: repository address %q starts with '-'", redact(repo))
	}
	if strings.Contains(repo, "://") {
		u, err := url.Parse(repo)
		if err != nil {
			return fmt.Errorf("workspace: repository address %q: %v", redact(repo), redactErr(err))
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "file":
		default:
			return fmt.Errorf("workspace: repository address %q: scheme %q is not accepted", redact(repo), u.Scheme)
		}
		if u.Host == "" && u.Scheme != "file" || u.Path == "" || u.Path == "/" {
			return fmt.Errorf("workspace: repository address %q has no host or path", redact(repo))
		}
		return nil
	}
	if scpLike.MatchString(repo) {
		return nil
	}
	return fmt.Errorf("workspace: repository address %q is neither a URL with a scheme nor user@host:path", redact(repo))
}

var userinfo = regexp.MustCompile(`(://)[^/@\s]+@`)

// redact removes credentials from an address or a message holding one.
func redact(s string) string { return userinfo.ReplaceAllString(s, "${1}***@") }

func redactErr(err error) string { return redact(err.Error()) }

// Make clones repo for thread. It implements the core's Workspaces.
func (m Maker) Make(ctx context.Context, thread control.ThreadID, repo string, placed func(core.Placement) error) (string, error) {
	return m.Clone(ctx, thread, repo, placed)
}

// Adopt returns the workspace of thread when it is the directory a Make
// that reported p put in place: the same path, device and inode. It looks
// only at the directory itself, and changes nothing. It implements the
// core's Workspaces.
func (m Maker) Adopt(ctx context.Context, thread control.ThreadID, p core.Placement) (string, bool, error) {
	// Where a Make of this release puts it, or, for a Make of a release
	// before thread numbers, where that one did: inside the workspaces
	// directory. Its device and inode are checked either way.
	target := Path(m.Root, thread)
	if p.Dir != target {
		rel, err := filepath.Rel(filepath.Join(m.Root, Dir), p.Dir)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsAbs(p.Dir) {
			return "", false, fmt.Errorf("workspace: %s is not the workspace of thread %d", p.Dir, thread)
		}
		target = p.Dir
	}
	st, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("workspace: %w", err)
	}
	dev, ino, ok := identity(st)
	if !st.IsDir() || !ok || dev != p.Dev || ino != p.Inode {
		return "", false, fmt.Errorf("%w: %s is not the directory this thread's clone made", ErrExists, target)
	}
	return target, true, nil
}

// identity is the device and inode of st.
func identity(st os.FileInfo) (dev, ino uint64, ok bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(sys.Dev), uint64(sys.Ino), true
}

// Clone makes the workspace of thread from repo's mirror when
// it can, falling back to repo. The workspace's origin is repo.
// The address is checked before git runs. The clone is made outside the
// workspaces directory and renamed into place when complete, so a failed
// clone never appears there. An existing directory is never touched.
// Right before the rename, placed, when not nil, is called with the clone's
// identity; when it fails, the clone is not renamed.
func (m Maker) Clone(ctx context.Context, thread control.ThreadID, repo string, placed func(core.Placement) error) (string, error) {
	if err := CheckAddress(repo); err != nil {
		return "", err
	}
	if thread <= 0 {
		return "", fmt.Errorf("workspace: no thread %d", thread)
	}
	target := Path(m.Root, thread)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("workspace: %w", err)
	}
	tmpRoot := filepath.Join(m.Root, cloneDir)
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	var r [8]byte
	rand.Read(r[:])
	tmp := filepath.Join(tmpRoot, hex.EncodeToString(r[:]))

	source, unlock := m.mirror(ctx, repo)
	defer unlock()
	cmd := gitCommand(ctx, "clone", "--quiet", "--", source, tmp)
	cmd.Env = m.gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("workspace: git clone %s failed: %s", redact(repo), lastLines(redact(stderr.String()), 3, redactErr(err)))
	}
	if source != repo {
		cmd := gitCommand(ctx, "-C", tmp, "remote", "set-url", "origin", repo)
		cmd.Env = m.gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("workspace: set origin: %s: %s", redactErr(err), redact(string(out)))
		}
	}
	if _, err := os.Lstat(filepath.Join(tmp, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("workspace: clone has alternates or cannot be checked")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	if placed != nil {
		st, err := os.Lstat(tmp)
		if err != nil {
			return "", fmt.Errorf("workspace: %w", err)
		}
		dev, ino, ok := identity(st)
		if !ok {
			return "", fmt.Errorf("workspace: %s has no device and inode", tmp)
		}
		// A rename keeps them: they identify the workspace at its place.
		if err := placed(core.Placement{Dir: target, Dev: dev, Inode: ino}); err != nil {
			return "", fmt.Errorf("workspace: %w (the clone is left at %s)", err, tmp)
		}
	}
	// A rename that never replaces: a directory that appeared meanwhile,
	// even an empty one, stays as it is.
	if err := renameNoReplace(tmp, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w: %s (the clone is left at %s)", ErrExists, target, tmp)
		}
		return "", fmt.Errorf("workspace: %w (the clone is left at %s)", err, tmp)
	}
	return target, nil
}

// gitEnv is the daemon's environment without what would point git at
// another repository, and without prompts.
func (m Maker) gitEnv() []string {
	environ := os.Environ
	if m.Environ != nil {
		environ = m.Environ
	}
	drop := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true,
		"GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_NAMESPACE": true, "GIT_CEILING_DIRECTORIES": true}
	var env []string
	for _, kv := range environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

func lastLines(s string, n int, fallback string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return fallback
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
