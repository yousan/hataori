package desktopstate

import (
	"os"
	"path/filepath"
	"testing"
)

// Actual filesystem evidence, in addition to the portable normalization
// policy fixtures. A case-sensitive runner must fail this acceptance item;
// it cannot silently skip or claim that Linux emulation proved APFS behavior.
func TestDarwinCaseInsensitiveScratch(t *testing.T) {
	root := rootFixture(t)
	if err := os.WriteFile(filepath.Join(root, "Collision"), []byte("inert"), 0o600); err != nil {
		t.Fatal(err)
	}
	one, err := os.Stat(filepath.Join(root, "Collision"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(filepath.Join(root, "collision"))
	if err != nil || !os.SameFile(one, two) {
		t.Fatal("case-insensitive disposable scratch volume is required")
	}
	if err := RefuseCollision(root, "collision"); err == nil {
		t.Fatal("case alias adopted")
	}
	if err := os.WriteFile(filepath.Join(root, "\u00e9"), []byte("inert Unicode"), 0o600); err != nil {
		t.Fatal(err)
	}
	one, err = os.Stat(filepath.Join(root, "\u00e9"))
	if err != nil {
		t.Fatal(err)
	}
	two, err = os.Stat(filepath.Join(root, "e\u0301"))
	if err != nil || !os.SameFile(one, two) {
		t.Fatal("normalization-insensitive disposable scratch volume is required")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "\u00e9" {
			if RefuseCollision(root, "e\u0301") == nil {
				t.Fatal("Unicode alias adopted")
			}
		}
		if entry.Name() == "e\u0301" {
			if RefuseCollision(root, "\u00e9") == nil {
				t.Fatal("Unicode alias adopted")
			}
		}
	}
}
