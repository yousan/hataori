package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if want != "chat_cursors,chat_inbound,chat_logins,chat_outbox,chat_triggers,events,inbox,login_outage_threads,login_outages,pending_inputs,requests,schema_version,session_files,sessions,sqlite_sequence,threads,turns,workspaces" {
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
	last := len(migrations) - 1
	if migrations[last].Name != "chat_triggers.thread" {
		t.Fatalf("the last step is %q", migrations[last].Name)
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
