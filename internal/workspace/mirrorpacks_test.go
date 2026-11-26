package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMirrorClearsOnlyOldUnusedTemporaryPacks(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	refresh := func() {
		dir, unlock := x.m.mirror(context.Background(), x.bare)
		unlock()
		if dir != x.mirror {
			t.Fatal("mirror unavailable")
		}
	}
	pack := filepath.Join(x.mirror, "objects", "pack")
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"tmp_pack_ABC123", "tmp_pack_NEW123", "tmp_pack_LINK12", "tmp_pack_HARD12", "tmp_pack_DIR123", "tmp_pack_other", "pack-keep.pack", "pack-keep.idx", "tmp_obj_ABC123"} {
		path := filepath.Join(pack, name)
		if err := os.WriteFile(path, []byte("unfinished"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "tmp_pack_NEW123" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Link(filepath.Join(pack, "tmp_pack_HARD12"), filepath.Join(x.tmp, "also")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pack, "tmp_pack_LINK12")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(pack, "pack-keep.pack"), link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(pack, "tmp_pack_DIR123")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(pack, "tmp_pack_ABC123")
	held, err := os.OpenFile(stale, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	refresh()
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("an open temporary pack was removed:", err)
	}
	held.Close()
	refresh()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an old unused temporary pack remains:", err)
	}
	for _, name := range []string{"tmp_pack_NEW123", "tmp_pack_LINK12", "tmp_pack_HARD12", "tmp_pack_DIR123", "tmp_pack_other", "pack-keep.pack", "pack-keep.idx", "tmp_obj_ABC123"} {
		if _, err := os.Lstat(filepath.Join(pack, name)); err != nil {
			t.Fatal(name, err)
		}
	}
}
