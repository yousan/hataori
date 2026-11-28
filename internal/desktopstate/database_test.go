package desktopstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/nativehttp"
)

func TestMainDatabaseRecoveryCannotRedirectOrAdoptOrphanWork(t *testing.T) {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		for _, kind := range []string{"orphan", "symlink", "hardlink", "writable", "alias"} {
			t.Run(suffix+kind, func(t *testing.T) {
				root := rootFixture(t)
				r, err := nativehttp.OpenPrivateRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				outside := filepath.Join(rootFixture(t), "only-copy")
				const original = "retain the only copy of this work"
				if err := os.WriteFile(outside, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				name := filepath.Join(root, "hataori.db"+suffix)
				if kind != "orphan" {
					os.WriteFile(filepath.Join(root, "hataori.db"), []byte("existing DB"), 0o600)
				}
				switch kind {
				case "orphan":
					os.WriteFile(name, []byte(original), 0o600)
				case "symlink":
					os.Symlink(outside, name)
				case "hardlink":
					os.Link(outside, name)
				case "writable":
					os.WriteFile(name, []byte(original), 0o622)
					os.Chmod(name, 0o622)
				case "alias":
					os.WriteFile(filepath.Join(root, "HATAORI.DB"+suffix), []byte(original), 0o600)
				}
				if _, err := CheckMainDatabase(r); err == nil {
					t.Fatal("unsafe recovery file accepted")
				}
				if data, _ := os.ReadFile(outside); string(data) != original {
					t.Fatal("outside-only-copy modified")
				}
				if kind == "orphan" {
					if data, _ := os.ReadFile(name); string(data) != original {
						t.Fatal("orphan work modified")
					}
				}
			})
		}
	}
}
