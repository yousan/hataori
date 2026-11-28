package workerinstall

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTrustKeySourceProtection(t *testing.T) {
	for _, name := range []string{"protected", "writable-key", "mutable-parent", "symlink"} {
		t.Run(name, func(t *testing.T) {
			parent := privateTestDir(t)
			path := filepath.Join(parent, "key")
			key := bytes.Repeat([]byte{42}, 32)
			if err := os.WriteFile(path, key, 0o644); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "writable-key":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "mutable-parent":
				if err := os.Chmod(parent, 0o777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := filepath.Join(parent, "alias")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			got, err := ReadTrustKey(path)
			if name == "protected" {
				if err != nil || !bytes.Equal(got, key) {
					t.Fatalf("protected key: %v", err)
				}
			} else if err == nil {
				t.Fatal("mutable trust source accepted")
			}
		})
	}
}
