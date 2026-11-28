package machineio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalReadFailuresAreNotEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "source")
	if err := os.WriteFile(p, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l := &Local{Dir: dir}
	src, err := l.Source(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Read(ctx, src, 0, MaxRead)
	if err != nil || string(b.Data) != "first\n" {
		t.Fatalf("initial: %q %v", b.Data, err)
	}
	b, err = l.Read(ctx, src, 6, MaxRead)
	if err != nil || len(b.Data) != 0 || b.Offset != 6 {
		t.Fatalf("empty: %+v %v", b, err)
	}
	wantState := func(state string) {
		t.Helper()
		b, err := l.Read(ctx, src, 6, MaxRead)
		var read *ReadError
		if !errors.As(err, &read) || read.State != state || len(b.Data) != 0 {
			t.Fatalf("want %s, got %+v %v", state, b, err)
		}
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	wantState("truncated")
	if err := os.Rename(p, p+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wantState("replaced")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	wantState("missing")
}
