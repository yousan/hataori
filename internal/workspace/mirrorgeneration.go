package workspace

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// selectedMirror returns the concrete generation, never a moving pathname.
// No pointer means the legacy mirror. A bad pointer authorizes no adoption.
func selectedMirrorPath(state, mirrors, key string) (string, error) {
	f, err := os.OpenFile(filepath.Join(state, key+".current"), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(mirrors, key+".git"), nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256 {
		return "", errors.New("workspace: invalid mirror generation pointer")
	}
	b, err := io.ReadAll(io.LimitReader(f, 257))
	name := strings.TrimSuffix(string(b), "\n")
	suffix, ok := strings.CutPrefix(name, key+"-generation-")
	suffix, end := strings.CutSuffix(suffix, ".git")
	if err != nil || !ok || !end || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
		return "", errors.New("workspace: invalid mirror generation pointer")
	}
	dir := filepath.Join(mirrors, name)
	if _, err := realDir(dir, false); err != nil {
		return "", err
	}
	return dir, nil
}

// mirrorLocked does not decide whether a lock is stale. Even a live Git may
// have closed its lock's descriptor while it waits to commit a transaction.
func mirrorLocked(ctx context.Context, dir string) (bool, error) {
	for _, name := range []string{"config.lock", "packed-refs.lock", "HEAD.lock"} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	var found bool
	err := filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}

// recoverMirror leaves the old directory and every lock untouched. Only this
// call's unpublished temporary clone may be removed. Completed generations,
// including one left orphaned by a crash, are never moved or cleaned up.
func (m Maker) recoverMirror(ctx context.Context, repo, state, mirrors, key string, lock *os.File) (string, error) {
	tmp, err := os.MkdirTemp(state, key+"-generation-")
	if err != nil {
		return "", err
	}
	defer func() {
		if tmp != "" {
			os.RemoveAll(tmp)
		}
	}()
	run := func(args ...string) error {
		cmd := gitCommand(ctx, args...)
		cmd.Env, cmd.ExtraFiles = m.gitEnv(), []*os.File{lock}
		return cmd.Run()
	}
	if err := run("clone", "--bare", "--no-hardlinks", "--", repo, tmp); err != nil {
		return "", err
	}
	if err := run("--git-dir", tmp, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
		return "", err
	}
	if err := run("--git-dir", tmp, "config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"); err != nil {
		return "", err
	}
	g := mirrorRepo{dir: tmp, env: m.gitEnv(), lock: lock}
	if err := g.guard(ctx); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(tmp, "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("workspace: recovered mirror has alternates or cannot be checked")
	}
	if err := syncMirror(ctx, tmp); err != nil {
		return "", err
	}
	name := filepath.Base(tmp) + ".git"
	dir := filepath.Join(mirrors, name)
	if err := renameNoReplace(tmp, dir); err != nil {
		return "", err
	}
	tmp = "" // neither the published directory nor its old pathname is cleaned up
	if err := syncMirrorDir(mirrors); err != nil {
		return "", err // the completed directory stays, even on publication failure
	}
	if err := publishMirror(state, key, name); err != nil {
		return "", err
	}
	return dir, nil
}

func publishMirror(state, key, name string) error {
	f, err := os.CreateTemp(state, key+"-pointer-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	_, err = io.WriteString(f, name+"\n")
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(state, key+".current")); err != nil {
		return err
	}
	tmp = ""
	return syncMirrorDir(state)
}

// Sync data before the pointer can survive a power failure. Refuse unexpected
// symlinks in this call's fresh clone; they are never followed or removed.
func syncMirror(ctx context.Context, dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("workspace: unexpected file in recovered mirror")
		}
		return syncMirrorDir(path)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncMirrorDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncMirrorDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
