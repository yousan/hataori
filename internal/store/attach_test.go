package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestAttachReservationOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	step := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == "sessions.attach_requested_at" })
	if step < 0 {
		t.Fatal("missing attach reservation migration")
	}
	s, err := open(ctx, path, migrations[:step])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'p', 'idle', 1, 2)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'fake', 'idle', 1, 2)`,
		`INSERT INTO turns (id, session_id, started_by, started_at, ended_at, status) VALUES (1, 1, 'caller', 1, 2, 'completion')`,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	want := dump(t, s.DB, `SELECT * FROM turns`)
	s.Close()
	s, err = open(ctx, path, migrations[:step+1])
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sameRows(t, "turn history", dump(t, s.DB, `SELECT * FROM turns`), want)
	sameRows(t, "attach reservation", dump(t, s.DB, `SELECT attach_requested_at FROM sessions`), []string{"0|"})
}
