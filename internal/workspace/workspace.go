// Package workspace makes the working directory of a thread: a clone of one
// repository, at a path of its own that nothing else uses.
//
// This package never resets a workspace in place. A mirror's branches and tags
// follow its remote's, and what they no longer reach is kept in it. A workspace
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
		if segmentKeeps(c) {
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

// segmentKeeps reports whether Segment keeps the byte c as it is.
func segmentKeeps(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
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

// ConfinedDir holds the directories of the threads started confined, such
// as a supervisor's (design, section 7.1), relative to the instance root.
// It is outside Dir, so no workspace is in one and none is in a workspace,
// and the Remover, which removes only below Dir, never removes one.
const ConfinedDir = "confined"

// ConfinedPath returns the directory of a thread started confined that was
// given none: <root>/confined/<number>. Supervisor threads started before
// each scope had a directory of its own have theirs there.
func ConfinedPath(root string, thread control.ThreadID) string {
	return filepath.Join(root, ConfinedDir, strconv.FormatInt(int64(thread), 10))
}

// scopeNameMax is the longest key ScopePath names a directory by in
// hex; a longer one is named by its hash, so the name fits in a path
// segment.
const scopeNameMax = 100

// ScopePath returns the directory of scope's supervisor threads (design,
// section 7.1, "Its directory and its notes") in <root>/confined/: one per
// scope key, so that no two scopes share one. A key of lowercase letters,
// digits, '-' and '_' only is the name after "scope-"; any other is
// hex-encoded after "scopex-", and one too long for that is the hex of its
// SHA-256 after "scopeh-". Keys that differ in case only, as some file
// systems do not tell apart, thus get two names. No name is a thread's
// number, as ConfinedPath's are.
func ScopePath(root, scope string) string {
	plain := scope != "" && len(scope) <= scopeNameMax
	for i := 0; i < len(scope) && plain; i++ {
		c := scope[i]
		plain = c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
	}
	name := "scope-" + scope
	switch {
	case plain:
	case len(scope) <= scopeNameMax:
		name = "scopex-" + hex.EncodeToString([]byte(scope))
	default:
		sum := sha256.Sum256([]byte(scope))
		name = "scopeh-" + hex.EncodeToString(sum[:])
	}
	return filepath.Join(root, ConfinedDir, name)
}

// Confined returns the directory of a thread started confined: dir, or
// ConfinedPath when dir is empty, which must be directly in
// <root>/confined/. The directory is made, empty, when it is not there. One
// that is there is used as it is, never emptied, moved or written to: it
// may be the directory of the scope's earlier supervisor threads, with
// their notes. A link at its place, or in place of confined/, is refused and
// left as it is. Any directory directly in confined/ is taken: that each
// scope's supervisor threads get their own is the supervisor client's
// choice of dir (supervisor.Sessions.Dir). It implements the core's
// Workspaces.
func (m Maker) Confined(ctx context.Context, thread control.ThreadID, dir string) (string, error) {
	if dir == "" {
		if thread <= 0 {
			return "", fmt.Errorf("workspace: no thread %d", thread)
		}
		dir = ConfinedPath(m.Root, thread)
	}
	parent := filepath.Join(m.Root, ConfinedDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	pst, err := os.Lstat(parent)
	if err != nil || !pst.IsDir() {
		return "", fmt.Errorf("workspace: %s is not a directory", parent)
	}
	dir = filepath.Clean(dir)
	base := filepath.Base(dir)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "", fmt.Errorf("workspace: %s is not a directory in %s", dir, parent)
	}
	// The directory it is in is confined/ itself, however the two are
	// spelled.
	if st, err := os.Lstat(filepath.Dir(dir)); err != nil || !os.SameFile(st, pst) {
		return "", fmt.Errorf("workspace: %s is not a directory in %s", dir, parent)
	}
	// From here on in the directory checked, held open, so that one put in
	// place of confined/ meanwhile is not followed.
	r, err := os.OpenRoot(parent)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	defer r.Close()
	if st, err := r.Stat("."); err != nil || !os.SameFile(st, pst) {
		return "", fmt.Errorf("workspace: %s was replaced", parent)
	}
	err = r.Mkdir(base, 0o700)
	if err == nil {
		return dir, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("workspace: %w", err)
	}
	st, err := r.Lstat(base)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrExists, dir)
	}
	return dir, nil
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
	if thread <= 0 {
		return "", fmt.Errorf("workspace: no thread %d", thread)
	}
	return m.cloneTo(ctx, repo, Path(m.Root, thread), placed)
}

// cloneTo is Clone at target, which is not touched when it exists.
func (m Maker) cloneTo(ctx context.Context, repo, target string, placed func(core.Placement) error) (string, error) {
	if err := CheckAddress(repo); err != nil {
		return "", err
	}
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
