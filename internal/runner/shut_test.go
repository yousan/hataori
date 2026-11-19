package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Shut closes a failed session only while it is its thread's newest: one
// that was replaced, as replace does when the agent cannot resume, is
// refused, and the thread, whose new session runs, is left as it is. So is
// an idle session whose last turn left background tasks running. A closed
// session is not closed again.
func TestShutRefusesAReplacedSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'owner', 'creating', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'test', 'failed', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (2, 1, 'test', 'creating', 0, 0)`,
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (2, 's', 't2', 'owner', 'failed', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (3, 2, 'test', 'failed', 0, 0)`,
		// Idle, with background tasks left running by its last turn.
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (3, 's', 't3', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (4, 3, 'test', 'idle', 0, 0)`,
		`INSERT INTO turns (session_id, started_by, started_at, ended_at, status, background) VALUES (4, 'caller', 0, 1, 'completion', 2)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{Store: st, Machine: &session.Machine{Store: st}, Root: root}
	state := func(table string, id int) string {
		var s string
		st.DB.QueryRow(`SELECT state FROM `+table+` WHERE id = ?`, id).Scan(&s)
		return s
	}
	if err := r.Shut(ctx, 1, "command"); !errors.Is(err, session.ErrRefused) {
		t.Fatalf("shut of a replaced session: %v, want refused", err)
	}
	if state("threads", 1) != "creating" || state("sessions", 1) != "failed" {
		t.Fatalf("after a refused shut: thread %s, session %s", state("threads", 1), state("sessions", 1))
	}
	if err := r.Shut(ctx, 4, "command"); !errors.Is(err, session.ErrRefused) || state("sessions", 4) != "idle" {
		t.Fatalf("shut of an idle session with background tasks: %v, state %s", err, state("sessions", 4))
	}
	if err := r.Shut(ctx, 3, "command"); err != nil {
		t.Fatal(err)
	}
	if state("threads", 2) != "closed" || state("sessions", 3) != "closed" {
		t.Fatalf("after shut: thread %s, session %s", state("threads", 2), state("sessions", 3))
	}
	var events int
	st.DB.QueryRow(`SELECT count(*) FROM events`).Scan(&events)
	if err := r.Shut(ctx, 3, "command"); err != nil {
		t.Fatalf("shut of a closed session: %v", err)
	}
	var again int
	st.DB.QueryRow(`SELECT count(*) FROM events`).Scan(&again)
	if again != events {
		t.Fatal("a closed session was closed again")
	}
}
