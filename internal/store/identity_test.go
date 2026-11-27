package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMainIdentityIsStableAndReadOnly(t *testing.T) {
	ctx := context.Background()
	s, root := machineStore(t)
	before, err := s.MainIdentity(ctx)
	if err != nil || before == "" {
		t.Fatalf("identity: %q %v", before, err)
	}
	read, err := ReadMainIdentity(ctx, root)
	if err != nil || read != before {
		t.Fatalf("read existing: %q %v", read, err)
	}
	other := filepath.Join(t.TempDir(), "not-an-instance")
	if _, err := ReadMainIdentity(ctx, other); err == nil {
		t.Fatal("absent instance accepted")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("read created missing instance: %v", err)
	}
}
