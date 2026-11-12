// Package workspace makes the working directory of a thread: a clone of one
// repository, at a path of its own that nothing else uses.
//
// This package creates directories and never removes or resets one.
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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yousan/hataori/internal/control"
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
	// Client names the client that owns a scope: the first path segment.
	Client func(scope string) string
	// Environ is the daemon's environment; os.Environ when nil.
	Environ func() []string
}

// Path returns where the workspace of (client, scope, key) is:
// <root>/workspaces/<client>/<scope>/<key>, each part made safe as one path
// segment.
func Path(root, client, scope, key string) string {
	return filepath.Join(root, Dir, Segment(client), Segment(scope), Segment(key))
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

var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^/\s][^\s]*$`)

// CheckAddress accepts a URL with a scheme and a host or path, or the SSH
// form user@host:path. It looks at the string only.
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
func (m Maker) Make(ctx context.Context, thread control.ThreadID, repo string) (string, error) {
	client := "client"
	if m.Client != nil {
		client = m.Client(thread.Scope)
	}
	return m.Clone(ctx, client, thread.Scope, thread.Key, repo)
}

// Clone makes the workspace of (client, scope, key) as a clone of repo.
// The address is checked before git runs. The clone is made outside the
// workspaces directory and renamed into place when complete, so a failed
// clone never appears there. An existing directory is never touched.
func (m Maker) Clone(ctx context.Context, client, scope, key, repo string) (string, error) {
	if err := CheckAddress(repo); err != nil {
		return "", err
	}
	target := Path(m.Root, client, scope, key)
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

	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--", repo, tmp)
	cmd.Env = m.gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("workspace: git clone %s failed: %s", redact(repo), lastLines(redact(stderr.String()), 3, redactErr(err)))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	// Rename does not replace a directory that holds anything, so a
	// workspace that appeared meanwhile stays as it is.
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%w: %s (the clone is left at %s)", ErrExists, target, tmp)
	}
	if err := os.Rename(tmp, target); err != nil {
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
