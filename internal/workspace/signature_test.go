package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRunsNoSignatureProgram(t *testing.T) {
	x := newRemoval(t)
	ran := filepath.Join(x.tmp, "ran")
	prog := filepath.Join(x.tmp, "gpg")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n: > \"${0%/*}/ran\"\ncat >/dev/null\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	x.git(x.clone, "config", "log.showSignature", "true")
	x.git(x.clone, "config", "gpg.program", prog)
	x.write("a", "work\n")
	x.git(x.clone, "stash", "-q")
	head, body, ok := strings.Cut(x.git(x.clone, "cat-file", "commit", "refs/stash"), "\n\n")
	if !ok {
		t.Fatal("stash has no commit body")
	}
	obj := filepath.Join(x.tmp, "obj")
	if err := os.WriteFile(obj, []byte(head+"\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n x\n -----END PGP SIGNATURE-----\n\n"+body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.git(x.clone, "stash", "store", "-m", "signed", x.git(x.clone, "hash-object", "-t", "commit", "-w", obj))
	if _, err := os.Lstat(ran); !os.IsNotExist(err) {
		t.Fatalf("signature program ran during setup, or marker unreadable: %v", err)
	}
	err := x.check()
	if _, statErr := os.Lstat(ran); !os.IsNotExist(statErr) {
		t.Fatalf("the check ran the configured signature program, or marker unreadable: %v", statErr)
	}
	// Disabling verification cannot make the stash safe to discard.
	wantRefusal(t, err, Stash, "")
}
