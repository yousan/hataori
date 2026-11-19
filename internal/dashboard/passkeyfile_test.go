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

// A passkey is not added once the file would be fuller than three
// quarters of what is read, which leaves room for sign-ins.
func TestAPasskeyLeavesRoomInTheFile(t *testing.T) {
	var keys []passkey
	k := passkey{ID: []byte("new"), Person: "ada", PublicKey: make([]byte, 77), Made: time.Now()}
	for len(keys)*1500 < maxPasskeysFile*3/4 {
		keys = append(keys, passkey{ID: []byte{byte(len(keys)), byte(len(keys) >> 8)}, Person: "grace", PublicKey: make([]byte, 1000), Made: time.Now()})
	}
	if err := addable(keys, k); !errors.Is(err, errFileFull) {
		t.Fatalf("adding to %d passkeys: %v, want the file full", len(keys), err)
	}
	if err := addable(keys[:10], k); err != nil {
		t.Fatalf("adding to 10 passkeys: %v", err)
	}
}
