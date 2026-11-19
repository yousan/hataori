package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A change that would make the passkeys file larger than load reads is
// refused, and the file as it was stays, so the daemon still starts; no
// temporary file is left.
func TestPasskeysFileNeverGrowsPastWhatItReads(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "state"), 0o700)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f := &passkeyFile{root: root}
	one := []passkey{{ID: []byte("id"), Person: "ada", PublicKey: make([]byte, 77), Made: time.Now()}}
	if err := f.save(one); err != nil {
		t.Fatal(err)
	}
	var many []passkey
	for i := 0; len(many)*1500 < maxPasskeysFile; i++ {
		many = append(many, passkey{ID: make([]byte, 255), Person: "ada", PublicKey: make([]byte, 800), Made: time.Now()})
	}
	if err := f.save(many); !errors.Is(err, errFileFull) {
		t.Fatalf("saving %d passkeys: %v, want the file to be refused", len(many), err)
	}
	keys, err := f.load()
	if err != nil || len(keys) != 1 {
		t.Fatalf("after the refused change: %d passkeys, %v", len(keys), err)
	}
	if _, err := os.Stat(filepath.Join(dir, PasskeysFile+".new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a temporary file is left: %v", err)
	}
}
