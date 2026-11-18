package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func tables(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.DB.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	return strings.Join(names, ",")
}

func TestOpenNewThenAgain(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if v, _ := s.Version(ctx); v != len(migrations) {
		t.Fatalf("version = %d, want %d", v, len(migrations))
	}
	var mode string
	s.DB.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %q", mode)
	}
	want := tables(t, s)
	if want != "chat_cursors,chat_inbound,chat_logins,chat_outbox,chat_triggers,events,inbox,login_outage_threads,login_outages,pending_inputs,requests,schema_version,session_files,session_reads,sessions,sqlite_sequence,threads,turns,workspaces" {
		t.Fatalf("tables = %s", want)
	}
	s.Close()

	s, err = Open(ctx, root)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s.Close()
	if got := tables(t, s); got != want {
		t.Fatalf("second Open changed the tables: %s", got)
	}
	if v, _ := s.Version(ctx); v != len(migrations) {
		t.Fatalf("version after second Open = %d", v)
	}
}

func create(name string) Migration {
	return Migration{Name: name, Up: execAll(`CREATE TABLE ` + name + ` (x INTEGER)`)}
}

func TestOlderVersionRunsLaterStepsOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := open(ctx, path, []Migration{create("a")})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Step 1 would fail if it ran again: table a exists.
	s, err = open(ctx, path, []Migration{create("a"), create("b")})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if v, _ := s.Version(ctx); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
	s.Close()

	failing := Migration{Name: "c then fail", Up: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE c (x INTEGER)`); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if _, err := open(ctx, path, []Migration{create("a"), create("b"), failing}); err == nil {
		t.Fatal("a failing migration succeeded")
	}
	s, err = open(ctx, path, []Migration{create("a"), create("b")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Version(ctx); v != 2 {
		t.Fatalf("version after failed step = %d, want 2", v)
	}
	if got := tables(t, s); got != "a,b,schema_version" {
		t.Fatalf("failed step left tables behind: %s", got)
	}
}

func TestNewerVersionIsRefusedUnmodified(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db?x#y")
	s, err := open(ctx, path, []Migration{create("a"), create("b")})
	if err != nil {
		t.Fatal(err)
	}
	// Leave it in another journal mode, so that switching to WAL would
	// modify it.
	if _, err := s.DB.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = open(ctx, path, []Migration{create("a")})
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open of a newer database: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("refused database was modified")
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatal("refused database has writes in its WAL")
	}
}

func TestConcurrentWritersAreNeverLocked(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, newRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deadline := time.Now().Add(10 * time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for g := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				tx, err := s.DB.BeginTx(ctx, nil)
				if err == nil {
					_, err = tx.Exec(`INSERT INTO threads (scope, key, started_by, state, created_at, updated_at) VALUES (?, ?, 'p', 'idle', 0, 0)`,
						"s", fmt.Sprintf("%d-%d", g, i))
					if err == nil {
						err = tx.Commit()
					} else {
						tx.Rollback()
					}
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer: %v", err)
	}
}

// Triggers stored before chat_triggers.thread keep the thread their start
// was accepted with; the others have none yet.
func TestTriggerThreadsKeptOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	last := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == "chat_triggers.thread" })
	if last < 0 {
		t.Fatal("no step chat_triggers.thread")
	}
	s, err := open(ctx, path, migrations[:last])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO chat_triggers (client, channel, message, text, received_at) VALUES ('chat', 'c1', 'm1', 't', 0), ('chat', 'c1', 'm2', 't', 0)`,
		`INSERT INTO inbox (key, data, accepted_at) VALUES ('start:c1' || char(0) || 'm1', CAST('{"scope":"c1","key":"th1"}' AS BLOB), 0)`,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = open(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := map[string]string{}
	rows, err := s.DB.QueryContext(ctx, `SELECT message, thread FROM chat_triggers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var m, th string
		rows.Scan(&m, &th)
		got[m] = th
	}
	if got["m1"] != "th1" || got["m2"] != "" || len(got) != 2 {
		t.Fatalf("threads %v, want m1 in th1 and m2 in none", got)
	}
}

// A database from before session_reads opens, keeps its sessions and
// offsets, its followed files start at 0 as they were read then, and it
// takes ranges after the step.
func TestSessionReadsAddedOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	step := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == "session_reads" })
	if step < 0 {
		t.Fatal("no step session_reads")
	}
	s, err := open(ctx, path, migrations[:step])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'owner', 'idle', 0, 0)`,
		`INSERT INTO sessions (id, thread_id, agent, state, log_offset, created_at, updated_at) VALUES (1, 1, 'a', 'idle', 120, 0, 0)`,
		`INSERT INTO session_files (session_id, path, offset) VALUES (1, 'transcript', 300)`,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = open(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Version(ctx); v != len(migrations) {
		t.Fatalf("version %d, want %d", v, len(migrations))
	}
	var log, file, start int64
	if err := s.DB.QueryRowContext(ctx, `SELECT log_offset, f.offset, f.start FROM sessions, session_files f WHERE id = 1 AND f.session_id = 1`).Scan(&log, &file, &start); err != nil || log != 120 || file != 300 || start != 0 {
		t.Fatalf("offsets %d %d, file start %d (%v), want 120 300 0", log, file, start, err)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM session_reads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d ranges (%v), want none", n, err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO session_reads (session_id, n, path, start, end) VALUES (1, 1, '', 120, 180)`); err != nil {
		t.Fatal(err)
	}
}
