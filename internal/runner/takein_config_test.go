package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/window"
)

// TakeIn persists resume configuration before launching its relay. Stop at
// a missing relay executable so this test needs neither tmux nor an agent.
func TestTakeInPersistsDefaultConfigOrigin(t *testing.T) {
	for _, defaultDir := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "default"}[defaultDir], func(t *testing.T) {
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
			} {
				if _, err := st.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			r := &Runner{Root: root, Store: st, Adapters: map[string]agent.Adapter{"test": nil}, Host: host.Command{Path: filepath.Join(t.TempDir(), "missing-host")}}
			rep := window.Report{Holder: window.Tmux, Server: filepath.Join(t.TempDir(), "socket") + ",42", Pane: "%1", PID: 123, Dir: t.TempDir(), Session: "conversation", ConfigDir: "/home/alice/.claude", ConfigDirDefault: defaultDir}
			if err := r.TakeIn(ctx, core.Launch{Session: 1, Thread: 1, Agent: "test"}, rep); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected only the missing relay: %v", err)
			}
			var b []byte
			if err := st.DB.QueryRow(`SELECT spec FROM sessions WHERE id = 1`).Scan(&b); err != nil {
				t.Fatal(err)
			}
			var saved storedSpec
			if err := json.Unmarshal(b, &saved); err != nil {
				t.Fatal(err)
			}
			// Resume clears TakenIn, but keeps this stored distinction.
			saved.Spec.TakenIn = false
			if saved.Spec.ConfigDir != rep.ConfigDir || saved.Spec.ConfigDirDefault != defaultDir {
				t.Fatalf("saved %+v", saved.Spec)
			}
		})
	}
}
