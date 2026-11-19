package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fixtureRoot is the instance root testdata/schema19.db was made under; its
// recorded paths start with it.
const fixtureRoot = "/tmp/hataori-fixture/root"

// fixtureDB copies testdata/name, a database a release made (README.md),
// into a new instance root, with the recorded paths moved from fixtureRoot
// to that root, and returns the root.
func fixtureDB(t *testing.T, name string) string {
	t.Helper()
	root := newRoot(t)
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, File)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE sessions SET spec = replace(spec, ?, ?), first_input = replace(first_input, ?, ?)`,
		fixtureRoot, root, fixtureRoot, root); err != nil {
		t.Fatal(err)
	}
	return root
}

// dump returns every row of the query, one line each.
func dump(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range v {
			p[i] = &v[i]
		}
		if err := rows.Scan(p...); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for i := range v {
			if x, ok := v[i].([]byte); ok {
				v[i] = string(x)
			}
			fmt.Fprintf(&b, "%v|", v[i])
		}
		out = append(out, b.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\ngot\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// tree records each entry under dir: its inode, mode, size and time.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		out[p] = fmt.Sprintf("dev %d ino %d mode %v size %d mtime %d", st.Dev, st.Ino, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The release before thread numbers made this database: three threads in
// two channels, their sessions, turns, events, outbox rows, catch-up
// cursors, triggers and inbox. The migration and LinkThreads give the
// threads of the configured channel a link each, fill the events' thread
// numbers, the outbox rows' links and the workspace paths, change no other
// row, and move no workspace.
func TestLinkThreadsOnTheReleasedDatabase(t *testing.T) {
	ctx := context.Background()
	root := fixtureDB(t, "schema19.db")

	// The workspaces where their sessions recorded them, with a file each.
	dirs := map[int64]string{
		1: filepath.Join(root, "workspaces", "fake-surface", "c1", "thread2"),
		2: filepath.Join(root, "workspaces", "fake-surface", "c1", "thread8"),
		3: filepath.Join(root, "workspaces", "fake-surface", "c2", "thread12"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "work.txt"), []byte("unpushed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := tree(t, root)
	delete(before, filepath.Join(root, "state")) // the database is written
	for p := range before {
		if strings.HasPrefix(p, filepath.Join(root, "state")+string(filepath.Separator)) {
			delete(before, p)
		}
	}

	old, err := sql.Open("sqlite", filepath.Join(root, File))
	if err != nil {
		t.Fatal(err)
	}
	const threadCols = `id, scope, key, title, repo, started_by, state, created_at, updated_at, tokens_turn, tokens_day, tokens_today, context_used, context_size`
	const outboxCols = `id, client, event, part, channel, thread, message, decoration, state, failures, ref, created_at, sent_at`
	unchanged := map[string]string{
		"threads":       `SELECT ` + threadCols + ` FROM threads ORDER BY id`,
		"events":        `SELECT pos, scope, thread, time, kind, data FROM events ORDER BY pos`,
		"chat_outbox":   `SELECT ` + outboxCols + ` FROM chat_outbox ORDER BY id`,
		"chat_inbound":  `SELECT * FROM chat_inbound ORDER BY client, channel, thread`,
		"chat_cursors":  `SELECT * FROM chat_cursors ORDER BY client`,
		"chat_triggers": `SELECT * FROM chat_triggers ORDER BY client, channel, message`,
		"inbox":         `SELECT * FROM inbox ORDER BY key`,
		"sessions":      `SELECT * FROM sessions ORDER BY id`,
		"turns":         `SELECT * FROM turns ORDER BY id`,
	}
	want := map[string][]string{}
	for table, q := range unchanged {
		want[table] = dump(t, old, q)
		if len(want[table]) == 0 {
			t.Fatalf("the fixture has no rows in %s", table)
		}
	}
	old.Close()

	s, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if v, _ := s.Version(ctx); v != len(migrations) {
		t.Fatalf("version %d, want %d", v, len(migrations))
	}
	var log bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&log, nil))
	// c2 is no longer in the config.
	if err := s.LinkThreads(ctx, map[string]string{"c1": "fake-surface"}, lg); err != nil {
		t.Fatalf("LinkThreads: %v", err)
	}

	for table, q := range unchanged {
		sameRows(t, table, dump(t, s.DB, q), want[table])
	}
	sameRows(t, "links", dump(t, s.DB, `SELECT id, thread_id, client, channel, thread, made_by, created_at FROM links ORDER BY id`), []string{
		"1|1|fake-surface|c1|thread2|fake-surface/hook-1|1795059777753|",
		"2|2|fake-surface|c1|thread8|fake-surface/hook-1|1795059778008|",
	})
	sameRows(t, "workspace paths", dump(t, s.DB, `SELECT id, workspace FROM threads ORDER BY id`), []string{
		"1|" + dirs[1] + "|", "2|" + dirs[2] + "|", "3|" + dirs[3] + "|",
	})
	sameRows(t, "events by thread", dump(t, s.DB, `SELECT scope, thread, thread_id, count(*) FROM events GROUP BY scope, thread, thread_id ORDER BY scope, thread`), []string{
		"c1|thread2|1|14|", "c1|thread8|2|8|", "c2|thread12|3|8|",
	})
	sameRows(t, "outbox links", dump(t, s.DB, `SELECT id, link FROM chat_outbox ORDER BY id`), []string{
		"1|1|", "2|1|", "3|1|", "4|2|", "5|2|", "6|<nil>|", "7|<nil>|",
	})
	if lines := strings.Split(strings.TrimSpace(log.String()), "\n"); len(lines) != 1 ||
		!strings.Contains(lines[0], "thread 3 gets no link") || !strings.Contains(lines[0], `\"c2\"`) {
		t.Fatalf("log:\n%s", log.String())
	}

	// Recorded, so that it does not run again, even with another config.
	log.Reset()
	if err := s.LinkThreads(ctx, map[string]string{"c1": "fake-surface", "c2": "fake-surface"}, lg); err != nil {
		t.Fatalf("second LinkThreads: %v", err)
	}
	if n := len(dump(t, s.DB, `SELECT id FROM links`)); n != 2 || log.Len() != 0 {
		t.Fatalf("second LinkThreads: %d links, log %q", n, log.String())
	}
	sameRows(t, "startup steps", dump(t, s.DB, `SELECT name FROM startup_steps ORDER BY name`), []string{
		"thread numbers: links|", "thread numbers: rows written before the API used them|"})

	// No workspace moved, was renamed or written; nothing was made beside them.
	after := tree(t, root)
	for p := range after {
		if p == filepath.Join(root, "state") || strings.HasPrefix(p, filepath.Join(root, "state")+string(filepath.Separator)) {
			delete(after, p)
		}
	}
	for p, w := range before {
		if after[p] != w {
			t.Errorf("%s: was %s, now %q", p, w, after[p])
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was made", p)
		}
	}

	// A conversation is no longer the thread's identity.
	if _, err := s.DB.Exec(`INSERT INTO threads (scope, key, started_by, state, created_at, updated_at) VALUES ('c1', 'thread2', 'x', 'idle', 0, 0)`); err != nil {
		t.Fatalf("a second thread with the same scope and key: %v", err)
	}
	var fk int
	s.DB.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatalf("foreign_keys = %d after the migration", fk)
	}
	// The foreign keys to threads still hold.
	if _, err := s.DB.Exec(`INSERT INTO sessions (thread_id, agent, state, created_at, updated_at) VALUES (999, 'fake', 'idle', 0, 0)`); err == nil {
		t.Fatal("a session of no thread was accepted")
	}
}

// The step runs once, also on a database with no threads.
func TestLinkThreadsNewDatabase(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, newRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lg := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if err := s.LinkThreads(ctx, nil, lg); err != nil {
		t.Fatal(err)
	}
	sameRows(t, "startup steps", dump(t, s.DB, `SELECT name FROM startup_steps ORDER BY name`), []string{
		"thread numbers: links|", "thread numbers: rows written before the API used them|"})
}

// The release with the migration but before the control API named threads
// by number made this database: its daemon ran the first step on a new
// database, then started one thread, whose link, events' thread number,
// outbox rows' link and workspace path it did not write. The second step
// writes them, once, and moves no workspace.
func TestLinkThreadsRowsWrittenBetween(t *testing.T) {
	ctx := context.Background()
	root := fixtureDB(t, "schema20-unlinked.db")
	dir := filepath.Join(root, "workspaces", "fake-surface", "c1", "thread2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("unpushed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := tree(t, filepath.Join(root, "workspaces"))

	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// As the release left it.
	sameRows(t, "steps before", dump(t, s.DB, `SELECT name FROM startup_steps`), []string{"thread numbers: links|"})
	sameRows(t, "links before", dump(t, s.DB, `SELECT id FROM links`), nil)
	sameRows(t, "workspace before", dump(t, s.DB, `SELECT id, workspace FROM threads`), []string{"1||"})
	sameRows(t, "events before", dump(t, s.DB, `SELECT count(*) FROM events WHERE thread_id IS NOT NULL`), []string{"0|"})
	sameRows(t, "outbox before", dump(t, s.DB, `SELECT count(*) FROM chat_outbox WHERE link IS NOT NULL`), []string{"0|"})
	events := dump(t, s.DB, `SELECT pos, scope, thread, time, kind, data FROM events ORDER BY pos`)
	if len(events) == 0 || len(dump(t, s.DB, `SELECT id FROM chat_outbox`)) == 0 {
		t.Fatal("the fixture has no events or outbox rows")
	}

	var log bytes.Buffer
	if err := s.LinkThreads(ctx, map[string]string{"c1": "fake-surface"}, slog.New(slog.NewTextHandler(&log, nil))); err != nil {
		t.Fatal(err)
	}
	sameRows(t, "links", dump(t, s.DB, `SELECT thread_id, client, channel, thread, made_by FROM links`), []string{
		"1|fake-surface|c1|thread2|fake-surface/hook-1|"})
	sameRows(t, "workspace", dump(t, s.DB, `SELECT id, workspace FROM threads`), []string{"1|" + dir + "|"})
	sameRows(t, "events", dump(t, s.DB, `SELECT count(*) FROM events WHERE thread_id IS NOT 1`), []string{"0|"})
	sameRows(t, "events unchanged", dump(t, s.DB, `SELECT pos, scope, thread, time, kind, data FROM events ORDER BY pos`), events)
	sameRows(t, "outbox", dump(t, s.DB, `SELECT count(*) FROM chat_outbox WHERE link IS NOT 1`), []string{"0|"})
	sameRows(t, "steps", dump(t, s.DB, `SELECT name FROM startup_steps ORDER BY name`), []string{
		"thread numbers: links|", "thread numbers: rows written before the API used them|"})
	if log.Len() != 0 {
		t.Fatalf("log: %s", log.String())
	}
	after := tree(t, filepath.Join(root, "workspaces"))
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("workspaces changed:\n%v\n%v", before, after)
	}
}

// A login outage's threads, kept by scope and key before thread numbers,
// are kept by number.
func TestLoginThreadsByNumber(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	s, err := open(ctx, filepath.Join(root, File), migrations[:20])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at) VALUES (4, 'c1', 'k4', 'p', 'idle', 0, 0), (7, 'c2', 'k7', 'p', 'idle', 0, 0)`,
		`INSERT INTO chat_logins (client, outage, agent, threads) VALUES ('x', 1, 'a', '[{"scope":"c1","key":"k4"},{"scope":"c2","key":"k7"}]'), ('x', 2, 'a', '[]')`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sameRows(t, "chat_logins", dump(t, s.DB, `SELECT outage, threads FROM chat_logins ORDER BY outage`), []string{"1|[4,7]|", "2|[]|"})
}
