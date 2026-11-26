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
	if want != "agent_limits,approvals,chat_cursors,chat_held,chat_inbound,chat_logins,chat_named,chat_notices,chat_outbox,chat_reopen_held,chat_senders,chat_triggers,conversation_marks,events,inbox,links,login_outage_threads,login_outages,names,pending_inputs,plugin_outages,proposals,requests,schema_version,session_files,session_reads,sessions,sqlite_sequence,startup_steps,supervisor_asked,supervisor_dirs,supervisor_docs,supervisor_files,supervisor_given,supervisor_reminders,supervisor_wakes,supervisor_writes,threads,trusted_files,turns,waiting_inputs,workspaces" {
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

// Of the links made before links.since, a thread's first one was opened for
// it, and everything in it is input; another counts from when it was made.
func TestLinksSinceOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	step := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == "links.since" })
	if step < 0 {
		t.Fatal("no step links.since")
	}
	s, err := open(ctx, path, migrations[:step])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'owner', 'idle', 0, 0), (2, 's', 't2', 'owner', 'idle', 0, 0)`,
		`INSERT INTO links (thread_id, client, channel, thread, created_at) VALUES (1, 'chat', 's', 't1', 10), (2, 'chat', 's', 't2', 20),
			(1, 'chat', 's', 'attached', 30), (2, 'chat', 's', 'attached too', 40), (1, 'chat', 'c9', 'opened', 50)`,
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
	sameRows(t, "links", dump(t, s.DB, `SELECT thread, since FROM links ORDER BY id`),
		[]string{"t1|0|", "t2|0|", "attached|30|", "attached too|40|", "opened|50|"})
}

// Of the links made before links.opened, a thread's first one counts as
// opened by its client and no other one does, whichever client's it is. An
// archive of a chat thread that does not count, which was written and not
// sent yet, is dropped; no other row of the outbox changes (#870).
func TestLinksOpenedOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	step := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == "links.opened" })
	if step < 0 {
		t.Fatal("no step links.opened")
	}
	s, err := open(ctx, path, migrations[:step])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (1, 's', 't1', 'owner', 'idle', 0, 0),
			(2, 's', 't2', 'owner', 'closed', 0, 0), (3, 's', 't3', 'owner', 'removed', 0, 0), (4, 's', 't4', 'owner', 'idle', 0, 0)`,
		// The links' times and since say nothing of which was first; thread
		// 3's first link is of another client, and thread 4 has none.
		`INSERT INTO links (id, thread_id, client, channel, thread, created_at, since) VALUES (1, 1, 'chat', 's', 't1', 90, 0), (2, 2, 'chat', 's', 't2', 80, 80),
			(3, 1, 'chat', 's', 'attached', 30, 30), (4, 2, 'chat', 's', 'attached too', 40, 40), (5, 1, 'chat', 'c9', 'opened', 50, 0),
			(6, 1, 'two', 'd1', 'x1', 60, 0), (7, 3, 'two', 'd1', 'x2', 70, 0), (8, 3, 'chat', 's', 't3', 75, 0)`,
		`INSERT INTO chat_outbox (id, client, event, part, channel, thread, message, state, created_at, link) VALUES
			(1, 'chat', 1, 1, 's', 't1', '{"op":"archive"}', 'pending', 0, 1),
			(2, 'chat', 1, 1, 's', 'attached', '{"op":"archive"}', 'pending', 0, 3),
			(3, 'chat', 2, 1, 's', 'attached', '{"op":"unarchive"}', 'pending', 0, 3),
			(4, 'chat', 3, 1, 's', 'attached', '{"op":"archive"}', 'sent', 0, 3),
			(5, 'chat', 4, 1, 's', 'attached', '{"op":"archive"}', 'dropped', 0, 3),
			(6, 'chat', 5, 1, 's', 'attached', '{"message":{"text":"archive"}}', 'pending', 0, 3),
			(7, 'chat', 6, 1, 's', 'attached', 'no JSON', 'pending', 0, 3),
			(8, 'two', 7, 1, 'd1', 'x2', '{"op":"archive"}', 'pending', 0, 7),
			(9, 'chat', 8, 1, 's', 'elsewhere', '{"op":"archive"}', 'pending', 0, 0),
			(10, 'chat', 9, 1, 's', 't2', '{"op":"archive"}', 'pending', 0, 0),
			(11, 'chat', 10, 1, 's', 'attached', '{"op":"archive"}', 'pending', 0, 0),
			(12, 'chat', 11, 1, 's', 'attached', '{"op":"archive"}', 'pending', 0, 1),
			(13, 'two', 12, 1, 's', 't1', '{"op":"archive"}', 'pending', 0, 0),
			(14, 'chat', 13, 1, 'c9', 't1', '{"op":"archive"}', 'pending', 0, 0),
			(15, 'chat', 14, 1, 's', 't1', '{"op":"archive"}', 'pending', 0, 3)`,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	outbox := func() []string {
		t.Helper()
		return dump(t, s.DB, `SELECT id, client, event, part, channel, thread, message, state, created_at, link FROM chat_outbox ORDER BY id`)
	}
	want := outbox()
	// Of one that no link counts as opened, whatever link the row names.
	for _, id := range []int{2, 9, 11, 12, 13, 14} {
		want[id-1] = strings.Replace(want[id-1], "|pending|", "|dropped|", 1)
	}
	s.Close()
	s, err = open(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sameRows(t, "links", dump(t, s.DB, `SELECT thread, opened FROM links ORDER BY id`),
		[]string{"t1|1|", "t2|1|", "attached|0|", "attached too|0|", "opened|0|", "x1|0|", "x2|1|", "t3|0|"})
	sameRows(t, "the outbox", outbox(), want)
}

// Of the threads from before threads.kind, those started confined are their
// scope's supervisor's own and the others ordinary; nothing else of any row
// changes (#867).
func TestThreadKindsOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	step := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == "threads.kind" })
	if step < 0 {
		t.Fatal("no step threads.kind")
	}
	s, err := open(ctx, path, migrations[:step])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, title, repo, started_by, state, created_at, updated_at, confined, workspace) VALUES
			(1, 's', 't1', 'a', 'r', 'owner', 'idle', 1, 2, 0, 'd1'), (2, 's', 't2', 'b', '', 'owner', 'closed', 3, 4, 1, 'd2'),
			(3, 's', 't3', 'c', '', 'owner', 'asleep', 5, 6, 1, 'd2'), (4, 'o', 't4', 'd', 'r', 'owner', 'removed', 7, 8, 0, ''),
			(5, 'o', 't5', 'e', '', 'owner', 'removed', 9, 10, 1, '')`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 3, 'a', 'asleep', 0, 0)`,
		`INSERT INTO events (pos, scope, thread, thread_id, time, kind, data) VALUES (7, 's', 't3', 3, 0, 'thread_created', '{"session":1}')`,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Every row of every table, without the column that is added.
	all := func() map[string][]string {
		t.Helper()
		out := map[string][]string{}
		for _, table := range dump(t, s.DB, `SELECT name FROM sqlite_master WHERE type = 'table' AND name != 'schema_version'`) {
			table = strings.TrimSuffix(table, "|")
			cols := dump(t, s.DB, `SELECT '"' || name || '"' FROM pragma_table_info('`+table+`') WHERE '`+table+`.' || name != 'threads.kind'`)
			for i := range cols {
				cols[i] = strings.TrimSuffix(cols[i], "|")
			}
			out[table] = append(cols, dump(t, s.DB, `SELECT `+strings.Join(cols, ", ")+` FROM "`+table+`" ORDER BY rowid`)...)
		}
		return out
	}
	before := all()
	s.Close()
	s, err = open(ctx, path, migrations[:step+1])
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sameRows(t, "the kinds", dump(t, s.DB, `SELECT id, kind FROM threads ORDER BY id`),
		[]string{"1|ordinary|", "2|supervisor|", "3|supervisor|", "4|ordinary|", "5|supervisor|"})
	after := all()
	for table, rows := range after {
		sameRows(t, table, rows, before[table])
	}
	if len(after) != len(before) {
		t.Fatalf("%d tables, %d before", len(after), len(before))
	}
}
