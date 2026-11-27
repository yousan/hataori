package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yousan/hataori/internal/core"
)

// CloneRegistered records the private staging directory and its identity before
// publication. The caller must durably commit that record before returning nil.
func (m Maker) CloneRegistered(ctx context.Context, handle, repo string, staged func(string, core.Placement) error) (string, error) {
	if !opaqueName(handle) || staged == nil {
		return "", errors.New("workspace: registered clone requires handle and journal callback")
	}
	return m.cloneStaged(ctx, repo, filepath.Join(m.Root, Dir, handle), nil, func(stage string, p core.Placement) error {
		if err := syncClone(stage); err != nil {
			return err
		}
		if err := syncParents(stage, p.Dir); err != nil {
			return err
		}
		return staged(stage, p)
	})
}

// Persist clone contents before committing their identity. Symlinks are
// directory entries, never followed; nonregular objects refuse publication.
func syncClone(path string) error {
	return filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("workspace: unsupported clone object")
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

func syncParents(source, destination string) error {
	for _, name := range []string{filepath.Dir(source), filepath.Dir(destination), filepath.Dir(filepath.Dir(source)), filepath.Dir(filepath.Dir(destination))} {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func opaqueName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func samePlacement(path string, p core.Placement) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	dev, ino, ok := identity(info)
	return ok && dev == p.Dev && ino == p.Inode && ino != 0
}
func direct(root, area, path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) == filepath.Join(root, area)
}
func ownedParent(path string) error {
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return errors.New("workspace: registered parent changed")
	}
	return nil
}

// RecoverClone never reruns git or adopts a directory by name. Only the exact
// journaled staging inode can be published, with an exclusive rename.
func (m Maker) RecoverClone(ctx context.Context, stage string, p core.Placement) error {
	root, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		return err
	}
	if !direct(root, cloneDir, stage) || !direct(root, Dir, p.Dir) || !opaqueName(filepath.Base(p.Dir)) {
		return errors.New("workspace: invalid registered clone location")
	}
	if err := ownedParent(filepath.Dir(stage)); err != nil {
		return err
	}
	if err := ownedParent(filepath.Dir(p.Dir)); err != nil {
		return err
	}
	if !samePlacement(stage, p) {
		return errors.New("workspace: registered clone identity changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameNoReplace(stage, p.Dir); err != nil {
		return err
	}
	if !samePlacement(p.Dir, p) {
		return errors.New("workspace: clone replaced during publication")
	}
	return syncParents(stage, p.Dir)
}

// RemoveRegistered reruns the unchanged complete proof on this machine before
// the rename. entry is journaled by the caller first and deliberately cannot be
// selected by the legacy time-name purge. No cached inspection grants removal.
func (r Remover) RemoveRegistered(ctx context.Context, dir, entry string, p core.Placement) error {
	trashMu.Lock()
	defer trashMu.Unlock()
	abs, root, err := r.place(dir)
	if err != nil {
		return err
	}
	if p.Dir != abs || !samePlacement(abs, p) || !direct(root, TrashDir, entry) || !strings.HasPrefix(filepath.Base(entry), "worker-") || !opaqueName(filepath.Base(entry)) {
		return errors.New("workspace: invalid registered removal identity/location")
	}
	if _, err := realDir(filepath.Join(root, TrashDir), true); err != nil {
		return err
	}
	if err := ownedParent(filepath.Dir(entry)); err != nil {
		return err
	}
	if err := r.check(ctx, abs, root); err != nil {
		return err
	}
	if !samePlacement(abs, p) {
		return errors.New("workspace: workspace replaced during proof")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameNoReplace(abs, entry); err != nil {
		return err
	}
	if !samePlacement(entry, p) {
		// Preserve the swapped directory, putting it back only without replacement.
		_ = renameNoReplace(entry, abs)
		return errors.New("workspace: workspace replaced during removal")
	}
	return syncParents(abs, entry)
}

func (r Remover) RestoreRegistered(ctx context.Context, entry string, p core.Placement) error {
	trashMu.Lock()
	defer trashMu.Unlock()
	root, err := filepath.EvalSymlinks(r.Maker.Root)
	if err != nil {
		return err
	}
	if !direct(root, TrashDir, entry) || !strings.HasPrefix(filepath.Base(entry), "worker-") || !opaqueName(filepath.Base(entry)) || !direct(root, Dir, p.Dir) || !opaqueName(filepath.Base(p.Dir)) {
		return errors.New("workspace: invalid registered restore location")
	}
	if err := ownedParent(filepath.Dir(entry)); err != nil {
		return err
	}
	if !samePlacement(entry, p) {
		return errors.New("workspace: trash identity changed")
	}
	target, err := r.placeBack(root, p.Dir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ownedParent(filepath.Dir(target)); err != nil {
		return err
	}
	if err := renameNoReplace(entry, target); err != nil {
		return err
	}
	if !samePlacement(target, p) {
		return errors.New("workspace: restored directory replaced")
	}
	return syncParents(entry, target)
}

// PurgeRegistered accepts only an identity previously journaled and authorized
// by the worker's acknowledgment/retention gate. Ambiguous replacements stay.
func (r Remover) PurgeRegistered(entry string, p core.Placement) error {
	trashMu.Lock()
	defer trashMu.Unlock()
	root, err := filepath.EvalSymlinks(r.Maker.Root)
	if err != nil {
		return err
	}
	if !direct(root, TrashDir, entry) || !strings.HasPrefix(filepath.Base(entry), "worker-") || !opaqueName(filepath.Base(entry)) {
		return errors.New("workspace: invalid registered purge location")
	}
	if err := ownedParent(filepath.Dir(entry)); err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Join(root, TrashDir))
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Lstat(filepath.Base(entry))
	if err != nil {
		return err
	}
	dev, ino, ok := identity(info)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || dev != p.Dev || ino != p.Inode || ino == 0 {
		return errors.New("workspace: registered purge identity changed")
	}
	// Open the exact journaled inode. Remove children through it, never through
	// a top-level pathname that could have become another directory meanwhile.
	entryRoot, err := parent.OpenRoot(filepath.Base(entry))
	if err != nil {
		return err
	}
	defer entryRoot.Close()
	opened, err := entryRoot.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("workspace: purge entry changed while opening")
	}
	directory, err := entryRoot.Open(".")
	if err != nil {
		return err
	}
	children, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := entryRoot.RemoveAll(child.Name()); err != nil {
			return fmt.Errorf("workspace: registered purge: %w", err)
		}
	}
	// Retain the empty inode as a tombstone. POSIX has no inode-conditional
	// rmdir; unlinking its pathname could remove an unrelated replacement.

	f, err := entryRoot.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// FetchRegistered refreshes the worker's non-pruning mirror and the independent
// checkout from the configured repository, never a main-supplied URL/origin.
func (m Maker) FetchRegistered(ctx context.Context, dir, repository string) error {
	if err := CheckAddress(repository); err != nil {
		return err
	}
	gitDir := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace: registered fetch requires its own Git directory")
	}
	_, unlock := m.mirror(ctx, repository)
	defer unlock()
	command := gitCommand(ctx, "-C", dir, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "--no-prune", "--no-prune-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", repository, "+refs/heads/*:refs/remotes/origin/*")
	command.Env = m.gitEnv()
	if command.Run() != nil {
		return errors.New("workspace: configured repository fetch failed")
	}
	return syncClone(gitDir)
}
