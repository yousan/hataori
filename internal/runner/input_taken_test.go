package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/store"
)

func TestClearReportedInputTaken(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'test', 'idle', 0, 0)`,
		`INSERT INTO pending_inputs (id, session_id, data) VALUES (42, 1, '{}'), (43, 1, '{}')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	err = st.Update(ctx, func(tx *store.Tx) error {
		return clearReported(ctx, tx, 1, []agent.Event{{Kind: agent.InputTaken, Input: "42"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE id = 42`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("input 42: count %d, error %v", n, err)
	}
	if err := st.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE id = 43`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("input 43: count %d, error %v", n, err)
	}
}
