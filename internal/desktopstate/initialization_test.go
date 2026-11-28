package desktopstate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCanceledAndInterruptedInitializationCanReopen(t *testing.T) {
	root := rootFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, err := Open(ctx, root); err == nil {
		s.Close()
		t.Fatal("canceled open succeeded")
	}
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal("canceled initialization prevented reopening:", err)
	}
	s.Close()
	// Simulate a crash after the permanent reservation and database creation,
	// before SQLite's first header/application-id transaction was durable.
	other := rootFixture(t)
	dir := filepath.Join(other, "state/desktop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "launcher.initialization"), []byte(initializationMagic), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "launcher.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), other)
	if err != nil {
		t.Fatal("owned interrupted initialization prevented reopening:", err)
	}
	s.Close()
}
