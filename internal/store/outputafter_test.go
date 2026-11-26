package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOutputBoundaryMigrationPreservesExistingLinks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	st, err := open(ctx, path, migrations[:len(migrations)-1])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 'scope', 'key', 'owner', 'idle', 0, 0)`,
		`INSERT INTO links (thread_id, client, channel, thread, created_at, since, opened) VALUES (1, 'chat', 'channel', 'thread', 123, 456, 1)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			st.Close()
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = open(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var created, since, opened, after int64
	if err := st.DB.QueryRow(`SELECT created_at, since, opened, output_after FROM links WHERE thread_id = 1`).Scan(&created, &since, &opened, &after); err != nil {
		t.Fatal(err)
	}
	if created != 123 || since != 456 || opened != 1 || after != 0 {
		t.Fatalf("migrated link: created %d, since %d, opened %d, output_after %d", created, since, opened, after)
	}
}
