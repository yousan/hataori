//go:build darwin && !cgo

package nativehttp

import (
	"errors"
	"os"
	"testing"
)

func TestDarwinWithoutACLVerifierRefuses(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "native-disabled-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if l, err := Listen(Config{Root: root, Instance: "test", Version: "test"}); !errors.Is(err, ErrACLUnavailable) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("native listener without ACL proof: %v", err)
	}
}
