package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// MirrorKey returns a safe, credential-free key for a remote.
func MirrorKey(repo string) string {
	normalized := repo
	if strings.Contains(repo, "://") {
		if u, err := url.Parse(repo); err == nil {
			if u.Scheme == "file" {
				normalized = localAddress(u.Path)
			} else {
				host := strings.ToLower(u.Hostname())
				port := u.Port()
				defaults := map[string]string{"https": "443", "http": "80", "ssh": "22", "git": "9418"}
				if port != "" && port != defaults[u.Scheme] {
					host = net.JoinHostPort(host, port)
				}
				normalized = "remote:" + host + "/" + remotePath(u.Path)
			}
		}
	} else if scpLike.MatchString(repo) {
		host, path, _ := strings.Cut(repo, ":")
		if _, h, ok := strings.Cut(host, "@"); ok {
			host = h
		}
		normalized = "remote:" + strings.ToLower(host) + "/" + remotePath(path)
	} else {
		normalized = localAddress(repo)
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func remotePath(path string) string {
	return strings.TrimSuffix(strings.Trim(path, "/"), ".git")
}

func localAddress(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	return "local:" + filepath.Clean(path)
}

// mirror returns a source and a function that releases its lock.
func (m Maker) mirror(ctx context.Context, repo string) (string, func()) {
	fallback := func() (string, func()) { return repo, func() {} }
	timeout := m.FetchTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	key := MirrorKey(repo)
	state := filepath.Join(m.Root, "state", "mirrors-tmp")
	if os.MkdirAll(state, 0o700) != nil {
		return fallback()
	}
	lock, err := os.OpenFile(filepath.Join(state, key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fallback()
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			lock.Close()
			return fallback()
		}
		select {
		case <-bounded.Done():
			lock.Close()
			return fallback()
		case <-time.After(10 * time.Millisecond):
		}
	}
	unlock := func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN); _ = lock.Close() }
	mirrors := filepath.Join(m.Root, "mirrors")
	mirror, err := selectedMirrorPath(state, mirrors, key)
	if err != nil {
		unlock()
		return fallback()
	}
	run := func(args ...string) error {
		cmd := gitCommand(bounded, args...)
		cmd.Env = m.gitEnv()
		return cmd.Run()
	}
	usable := func() bool { return run("--git-dir", mirror, "rev-parse", "--is-bare-repository") == nil }
	if _, err := os.Lstat(mirror); errors.Is(err, os.ErrNotExist) {
		tmp, err := os.MkdirTemp(state, key+"-")
		if err == nil {
			// Only this call's half-built mirror is removed; it holds nobody's work.
			defer os.RemoveAll(tmp)
			if run("clone", "--bare", "--", repo, tmp) == nil &&
				run("--git-dir", tmp, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*") == nil &&
				run("--git-dir", tmp, "config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*") == nil &&
				os.MkdirAll(filepath.Dir(mirror), 0o700) == nil {
				_ = renameNoReplace(tmp, mirror)
			}
		}
	} else if usable() {
		// A lock can belong to a live manual Git that never took our flock.
		// Recover elsewhere; never remove its lock or move its directory.
		locked, err := mirrorLocked(bounded, mirror)
		if err == nil && !locked {
			mirrorRepo{dir: mirror, env: m.gitEnv(), lock: lock}.update(bounded, repo)
			locked, err = mirrorLocked(bounded, mirror) // an update killed at its deadline
		}
		if err == nil && locked {
			if next, err := m.recoverMirror(bounded, repo, state, mirrors, key, lock); err == nil {
				mirror = next
			}
		}
	}
	// The fetch deadline may have expired. Inspect locally using the caller's context.
	cmd := gitCommand(ctx, "--git-dir", mirror, "rev-parse", "--is-bare-repository")
	cmd.Env = m.gitEnv()
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		return mirror, unlock
	}
	unlock()
	return fallback()
}
