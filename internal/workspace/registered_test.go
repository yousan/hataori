package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/core"
)

func TestRegisteredRemovalRefusesExternalAndConfinedDirectories(t *testing.T) {
	for _, area := range []string{"external", "confined"} {
		t.Run(area, func(t *testing.T) {
			x := newRemoval(t)
			dir := filepath.Join(x.tmp, "external")
			if area == "confined" {
				dir = filepath.Join(x.root, ConfinedDir, "scope-test")
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			x.git(x.tmp, "clone", "-q", x.bare, dir)
			marker := filepath.Join(dir, "a")
			info, _ := os.Stat(dir)
			dev, ino, _ := identity(info)
			placement := core.Placement{Dir: dir, Dev: dev, Inode: ino}
			entry := filepath.Join(x.root, TrashDir, "worker-fixture")
			if err := x.r.RemoveRegistered(context.Background(), dir, entry, placement); err == nil {
				t.Fatal("non-workspace moved")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "one\n" {
				t.Fatal("external/confined data lost", err)
			}
		})
	}
}

func TestRegisteredPurgeRefusesUnknownIdentityAndExternalEntries(t *testing.T) {
	x := newRemoval(t)
	for _, kind := range []string{"different_inode", "outside_trash", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			entry := filepath.Join(x.root, TrashDir, "worker-"+kind)
			if kind == "outside_trash" {
				entry = filepath.Join(x.root, Dir, "worker-outside")
			}
			os.MkdirAll(entry, 0700)
			marker := filepath.Join(entry, "only-copy")
			os.WriteFile(marker, []byte("keep"), 0600)
			info, _ := os.Stat(entry)
			dev, ino, _ := identity(info)
			p := core.Placement{Dir: x.clone, Dev: dev, Inode: ino}
			if kind == "different_inode" {
				p.Inode++
			}
			if kind == "symlink" {
				os.Rename(entry, entry+"-original")
				os.Symlink(entry+"-original", entry)
				marker = filepath.Join(entry+"-original", "only-copy")
			}
			if err := x.r.PurgeRegistered(entry, p); err == nil {
				t.Fatal("unowned purge accepted")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
				t.Fatal("unowned entry deleted", err)
			}
		})
	}
}

func TestRegisteredRestoreNeverReplacesEmptyDirectory(t *testing.T) {
	x := newRemoval(t)
	direct := filepath.Join(x.root, Dir, "registered-restore")
	if err := os.Rename(x.clone, direct); err != nil {
		t.Fatal(err)
	}
	x.clone = direct
	info, _ := os.Stat(x.clone)
	dev, ino, _ := identity(info)
	p := core.Placement{Dir: x.clone, Dev: dev, Inode: ino}
	entry := filepath.Join(x.root, TrashDir, "worker-restore")
	if err := x.r.RemoveRegistered(context.Background(), x.clone, entry, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(x.clone, 0700); err != nil {
		t.Fatal(err)
	}
	replacement, _ := os.Stat(x.clone)
	if err := x.r.RestoreRegistered(context.Background(), entry, p); err == nil {
		t.Fatal("empty replacement overwritten")
	}
	after, err := os.Stat(x.clone)
	if err != nil || !os.SameFile(replacement, after) {
		t.Fatal("replacement lost", err)
	}
	if !samePlacement(entry, p) {
		t.Fatal("original trash lost")
	}
}

func TestRegisteredCloneNeverReplacesEmptyDirectory(t *testing.T) {
	x := newRemoval(t)
	handle := "registered-clone"
	target := filepath.Join(x.root, Dir, handle)
	var replacement os.FileInfo
	_, err := x.r.Maker.CloneRegistered(context.Background(), handle, "file://"+x.bare, func(_ string, _ core.Placement) error {
		if err := os.Mkdir(target, 0700); err != nil {
			return err
		}
		replacement, _ = os.Stat(target)
		return nil
	})
	if err == nil {
		t.Fatal("clone overwrote empty replacement")
	}
	after, statErr := os.Stat(target)
	if statErr != nil || replacement == nil || !os.SameFile(replacement, after) {
		t.Fatal("clone replacement lost", statErr)
	}
}
