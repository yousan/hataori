package desktopstate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/store"
)

func TestSetupIdentityRetriesOnlyReservedInitializationAndNeverResetsEstablishedData(t *testing.T) {
	ctx := context.Background()
	root := rootFixture(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := setupDBMarker(s.state, "desktop.setup-database", true); err != nil {
		t.Fatal(err)
	}
	f, err := nativehttp.OpenPrivateFile(s.state, "hataori.db", os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	id, err := InitializeMainIdentity(ctx, root)
	if err != nil || id == "" {
		t.Fatal("reserved empty initialization could not resume", err)
	}
	again, err := InitializeMainIdentity(ctx, root)
	if err != nil || again != id {
		t.Fatal("installation identity changed", err)
	}
	// Truncating an established scratch DB simulates corruption, not permission
	// to initialize it again, even though the permanent reservation remains.
	if err := os.WriteFile(filepath.Join(root, store.File), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeMainIdentity(ctx, root); err == nil {
		t.Fatal("established database was silently recreated")
	}
	if info, _ := os.Stat(filepath.Join(root, store.File)); info.Size() != 0 {
		t.Fatal("corrupt established data was modified")
	}
}

func TestSetupIdentityPreservesUnknownAndOrphanDatabaseWork(t *testing.T) {
	for _, entry := range []string{"hataori.db", "hataori.db-journal", "hataori.db-wal", "hataori.db-shm"} {
		t.Run(entry, func(t *testing.T) {
			root := rootFixture(t)
			s, err := Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			name := filepath.Join(s.state.Name(), entry)
			const work = "unknown only-copy work"
			if err := os.WriteFile(name, []byte(work), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := InitializeMainIdentity(context.Background(), root); err == nil {
				t.Fatal("unknown work adopted")
			}
			if data, _ := os.ReadFile(name); string(data) != work {
				t.Fatal("unknown work changed")
			}
		})
	}
}
