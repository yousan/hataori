package workspace

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var temporaryPack = regexp.MustCompile(`^tmp_pack_[A-Za-z0-9]{6}$`)

// clearPacks removes only old temporary packs, with the mirror's lock held.
// A killed fetch cannot remove its temporary pack. gc.pruneExpire=never,
// which protects the mirror's objects, keeps that file forever too.
func (g mirrorRepo) clearPacks(ctx context.Context) {
	if g.lock == nil {
		return
	}
	dir := g.dir
	for _, part := range []string{"", "objects", "pack"} {
		dir = filepath.Join(dir, part)
		if _, err := realDir(dir, false); err != nil {
			return
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	old := map[string]os.FileInfo{}
	for _, entry := range entries {
		if !temporaryPack.MatchString(entry.Name()) {
			continue
		}
		st, err := entry.Info()
		if err == nil && st.Mode().IsRegular() && st.ModTime().Before(cutoff) {
			_, links := fileIDOf(st)
			if links == 1 {
				old[filepath.Join(dir, entry.Name())] = st
			}
		}
	}
	if len(old) == 0 {
		return
	}
	// Age and our lock do not exclude a git started by hand. Reuse the
	// removal check's process probe; an unreadable or busy store stays.
	c := checker{ctx: ctx, top: dir}
	if c.processes() != nil {
		return
	}
	for path, before := range old {
		if ctx.Err() != nil {
			return
		}
		after, err := os.Lstat(path)
		if err == nil && os.SameFile(before, after) && after.Mode().IsRegular() && after.ModTime().Equal(before.ModTime()) {
			_ = os.Remove(path)
		}
	}
}
