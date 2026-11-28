package desktopstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/wire"
)

// These files are inert byte fixtures, never executable children. Every path
// is below an independently created scratch directory.
func bundleFixture(t *testing.T, version string) string {
	t.Helper()
	dir := rootFixture(t)
	m := Manifest{Format: 1, Version: version, Commit: strings.Repeat("1", 40), Platform: "darwin", Arch: architecture(), Schema: store.SchemaVersion(), ControlProtocol: wire.Version, HostCapability: host.InputBatchVersion, Files: map[string]string{}}
	for _, name := range bundleFiles {
		data := []byte("inert " + name + " " + version)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		m.Files[name] = hex.EncodeToString(hash[:])
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "desktop-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stagedFixture(t *testing.T, s *State, version string) Selection {
	t.Helper()
	d, err := s.Stage(context.Background(), bundleFixture(t, version), version)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBundleSelectionVerifiesEveryStartAndPreservesOldVersions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, rootFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	one := stagedFixture(t, s, "1.0.0")
	if err := s.Register(ctx, one); err != nil {
		t.Fatal(err)
	}
	two := stagedFixture(t, s, "2.0.0")
	if err := s.Select(ctx, two.Version, two.Binary); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Read(ctx)
	if got.Intent != Stopped {
		t.Fatal("update undid stop")
	}
	if err := s.VerifySelection(ctx, one); err != nil {
		t.Fatal("old referenced bundle was not retained", err)
	}
	if err := s.Select(ctx, one.Version, filepath.Join(s.root.Name(), "elsewhere")); err == nil {
		t.Fatal("arbitrary stored binary accepted")
	}
	if err := os.WriteFile(two.Binary, []byte("changed after selection"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifySelection(ctx, got); err == nil {
		t.Fatal("changed executable accepted at launch")
	}
	still, _ := s.Read(ctx)
	if still != got {
		t.Fatal("verification changed persisted state")
	}
}

func TestBundleRefusesPartialChangedAndLinkedStages(t *testing.T) {
	for _, kind := range []string{"checksum", "source-link", "target-link", "partial", "version-reuse"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, rootFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			resources := bundleFixture(t, "1.0.0")
			switch kind {
			case "checksum":
				os.WriteFile(filepath.Join(resources, "hataori"), []byte("corrupt"), 0o700)
			case "source-link":
				os.Remove(filepath.Join(resources, "hataori"))
				os.Symlink("hataori-desktop-transport", filepath.Join(resources, "hataori"))
			case "target-link":
				os.Mkdir(filepath.Join(s.root.Name(), "bin"), 0o700)
				os.Symlink(resources, filepath.Join(s.root.Name(), "bin/1.0.0"))
			case "partial":
				os.MkdirAll(filepath.Join(s.root.Name(), "bin/1.0.0"), 0o700)
			case "version-reuse":
				if _, err := s.Stage(ctx, resources, "1.0.0"); err != nil {
					t.Fatal(err)
				}
				data, _ := os.ReadFile(filepath.Join(resources, "desktop-manifest.json"))
				var m Manifest
				json.Unmarshal(data, &m)
				m.Commit = strings.Repeat("2", 40)
				data, _ = json.Marshal(m)
				os.WriteFile(filepath.Join(resources, "desktop-manifest.json"), data, 0o600)
			}
			if _, err := s.Stage(ctx, resources, "1.0.0"); err == nil {
				t.Fatal("unsafe stage accepted")
			}
			if _, err := s.Read(ctx); err == nil {
				t.Fatal("failed staging registered an instance")
			}
		})
	}
}

func TestBundleRefusesSchemaRollbackWithoutChangingData(t *testing.T) {
	ctx := context.Background()
	root := rootFixture(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := stagedFixture(t, s, "1.0.0")
	db, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	file := filepath.Join(filepath.Dir(d.Binary), "desktop-manifest.json")
	data, _ := os.ReadFile(file)
	var m Manifest
	json.Unmarshal(data, &m)
	m.Schema--
	data, _ = json.Marshal(m)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, store.File))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifySelection(ctx, d); err == nil {
		t.Fatal("older schema accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, store.File))
	if string(before) != string(after) {
		t.Fatal("compatibility probe modified database")
	}
}
