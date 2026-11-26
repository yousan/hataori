package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Exercise the runner's actual commit and cleanup, without a process or
// terminal: a failed held input must not disappear or end another turn.
func TestPendingFailureOwnsOnlyItsInput(t *testing.T) {
	for _, state := range []string{"idle", "running", "waiting"} {
		for _, reason := range []agent.Reason{agent.NotDelivered, agent.NotTaken} {
			t.Run(state+"/"+string(reason), func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
					t.Fatal(err)
				}
				st, err := store.Open(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { st.Close() })
				for _, q := range []string{
					`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't', 'owner', '` + state + `', 0, 0)`,
					`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'test', '` + state + `', 0, 0)`,
					`INSERT INTO turns (id, session_id, started_by, principal, from_id, trigger, started_at, ended_at, status) VALUES (1, 1, 'caller', 'first', 'chat:first', 'm1', 0, 1, 'completion')`,
				} {
					if _, err := st.DB.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
				if state == "running" {
					if _, err := st.DB.Exec(`INSERT INTO turns (id, session_id, started_by, principal, from_id, trigger, started_at) VALUES (2, 1, 'caller', 'third', 'chat:third', 'm3', 2)`); err != nil {
						t.Fatal(err)
					}
				}
				p := session.PendingInput{Input: control.Input{Text: "plugin\nheld"}, Origin: &session.InputOrigin{Key: "m2", From: "chat:second", Principal: "second", Text: "held"}}
				data, _ := json.Marshal(p)
				if _, err := st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data) VALUES (42, 1, ?)`, string(data)); err != nil {
					t.Fatal(err)
				}
				r := &Runner{Store: st, Machine: &session.Machine{Store: st}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
				lv := &live{id: 1}
				b := batch{events: []agent.Event{{Kind: agent.TurnEnded, Status: agent.Failed, Reason: reason, Input: "42"}}}
				if _, err := st.DB.Exec(`CREATE TRIGGER fail_read BEFORE UPDATE OF log_offset ON sessions BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
					t.Fatal(err)
				}
				if _, err := r.commit(lv, b); err == nil {
					t.Fatal("commit succeeded past the injected failure")
				}
				var left, logged int
				if err := st.DB.QueryRow(`SELECT (SELECT count(*) FROM pending_inputs), (SELECT count(*) FROM events)`).Scan(&left, &logged); err != nil || left != 1 || logged != 0 {
					t.Fatalf("rollback: pending %d, events %d: %v", left, logged, err)
				}
				if _, err := st.DB.Exec(`DROP TRIGGER fail_read`); err != nil {
					t.Fatal(err)
				}
				if _, err := r.commit(lv, b); err != nil {
					t.Fatal(err)
				}
				var gotState, raw string
				if err := st.DB.QueryRow(`SELECT state FROM sessions`).Scan(&gotState); err != nil || gotState != state {
					t.Fatalf("state %q: %v", gotState, err)
				}
				if err := st.DB.QueryRow(`SELECT data FROM events WHERE kind = 'input_failed'`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var d map[string]any
				if err := json.Unmarshal([]byte(raw), &d); err != nil {
					t.Fatal(err)
				}
				for k, want := range map[string]any{"input": "42", "key": "m2", "from": "chat:second", "principal": "second", "text": "held", "reason": string(reason)} {
					if d[k] != want {
						t.Errorf("%s = %v, want %v", k, d[k], want)
					}
				}
				var n int
				if err := st.DB.QueryRow(`SELECT count(*) FROM pending_inputs`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("pending %d: %v", n, err)
				}
				// A duplicate failure after the row was removed is harmless too.
				if _, err := r.commit(lv, b); err != nil {
					t.Fatal(err)
				}
				if err := st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'input_failed'`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("failure events %d: %v", n, err)
				}
				if state == "running" {
					if err := st.DB.QueryRow(`SELECT count(*) FROM turns WHERE id = 2 AND ended_at IS NULL`).Scan(&n); err != nil || n != 1 {
						t.Fatalf("other open turn %d: %v", n, err)
					}
					if _, err := r.commit(lv, batch{events: []agent.Event{{Kind: agent.TurnEnded, Status: agent.Completed}}}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
