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
// an idle session whose last turn left background tasks running, and an
// idle session with input or an answer no turn has reported. A closed
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
		// A failed session runs no more turns: input left pending in it
		// does not keep it from being closed.
		`INSERT INTO pending_inputs (session_id, data) VALUES (3, '{"text":"left"}')`,
		// Idle, with background tasks left running by its last turn.
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (3, 's', 't3', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (4, 3, 'test', 'idle', 0, 0)`,
		`INSERT INTO turns (session_id, started_by, started_at, ended_at, status, background) VALUES (4, 'caller', 0, 1, 'completion', 2)`,
		// Idle, with input the core accepted that no turn has reported:
		// one not written to the agent yet, one written.
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (4, 's', 't4', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (5, 4, 'test', 'idle', 0, 0)`,
		`INSERT INTO pending_inputs (session_id, data, written) VALUES (5, '{"text":"held"}', 0)`,
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (5, 's', 't5', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (6, 5, 'test', 'idle', 0, 0)`,
		`INSERT INTO pending_inputs (session_id, data, written) VALUES (6, '{"text":"written"}', 1)`,
		// Idle, with an answer the host has not taken.
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (6, 's', 't6', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (7, 6, 'test', 'idle', 0, 0)`,
		`INSERT INTO pending_inputs (session_id, request, data) VALUES (7, 'r1', '{"text":"yes"}')`,
		// Idle, with no pending input.
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (7, 's', 't7', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (8, 7, 'test', 'idle', 0, 0)`,
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
	for _, s := range []int{5, 6, 7} {
		var before int
		st.DB.QueryRow(`SELECT count(*) FROM events`).Scan(&before)
		if err := r.Shut(ctx, int64(s), "command"); !errors.Is(err, session.ErrRefused) {
			t.Fatalf("shut of idle session %d with a pending input: %v, want refused", s, err)
		}
		var rows, after int
		st.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE session_id = ?`, s).Scan(&rows)
		st.DB.QueryRow(`SELECT count(*) FROM events`).Scan(&after)
		if state("sessions", s) != "idle" || state("threads", s-1) != "idle" || rows != 1 || after != before {
			t.Fatalf("after a refused shut of session %d: session %s, thread %s, %d pending rows, %d new events",
				s, state("sessions", s), state("threads", s-1), rows, after-before)
		}
	}
	if err := r.Shut(ctx, 8, "command"); err != nil || state("sessions", 8) != "closed" || state("threads", 7) != "closed" {
		t.Fatalf("shut of an idle session with no pending input: %v, session %s", err, state("sessions", 8))
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
