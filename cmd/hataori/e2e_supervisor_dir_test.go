package main

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// threadDir is the directory stored with thread id.
func threadDir(t *testing.T, root string, id control.ThreadID) string {
	t.Helper()
	var d string
	if err := dbOf(t, root).QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(id)).Scan(&d); err != nil || d == "" {
		t.Fatalf("thread %d has no directory: %v", id, err)
	}
	return d
}

// closeThread closes thread id with hataori close.
func closeThread(t *testing.T, root string, id control.ThreadID) {
	t.Helper()
	if code, _, errOut := cli(t, "close", "--root", root, strconv.FormatInt(int64(id), 10)); code != 0 {
		t.Fatalf("close: %d %s", code, errOut)
	}
}

// A scope's next supervisor thread, started after the last was closed,
// starts in the scope's directory and finds the file the last one's
// session wrote there.
func TestSupervisorDirectoryOutlivesItsThreads(t *testing.T) {
	r, _ := svRoot(t, true)
	stop := runDaemon(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	first := supervisorThread(t, owner, "c1", control.Idle)
	if a := sendAndRead(t, owner, first.ID, "w", "write notes/plan.md the plan of #1"); a != "wrote notes/plan.md" {
		t.Fatalf("the first supervisor's write: %q", a)
	}
	closeThread(t, r.root, first.ID)
	owner.Close()
	stop()

	stop = runDaemon(t, r.root)
	defer stop()
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	next := supervisorThread(t, owner, "c1", control.Idle)
	if next.ID == first.ID {
		t.Fatalf("no new supervisor thread after %d was closed", first.ID)
	}
	if a := sendAndRead(t, owner, next.ID, "r", "read notes/plan.md"); a != "read: the plan of #1" {
		t.Fatalf("the next supervisor read %q; want the last one's notes", a)
	}
	scope := workspace.ScopePath(r.root, "c1")
	if d1, d2 := threadDir(t, r.root, first.ID), threadDir(t, r.root, next.ID); d1 != scope || d2 != scope {
		t.Fatalf("the supervisor threads' directories are %s and %s; want both %s", d1, d2, scope)
	}
}

// A scope whose supervisor thread exists when the daemon first starts with
// a directory per scope, its directory being confined/<number> as before,
// keeps that directory: the thread goes on in it, the scope's next
// supervisor thread starts in it, and nothing in it, a file and a nested
// directory, changes.
func TestSupervisorKeepsTheDirectoryOfItsThreadFromBefore(t *testing.T) {
	r, _ := svRoot(t, true)
	stop := runDaemon(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	th := supervisorThread(t, owner, "c1", control.Idle)
	owner.Close()
	stop()

	// The store and the directory as a daemon from before left them: the
	// thread's directory confined/<number>, no directory per scope.
	old, now := workspace.ConfinedPath(r.root, th.ID), threadDir(t, r.root, th.ID)
	if err := os.Rename(now, old); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM supervisor_dirs`, `UPDATE threads SET workspace = ?2 WHERE workspace = ?1`,
		`UPDATE sessions SET spec = replace(spec, ?1, ?2), first_input = replace(first_input, ?1, ?2)`} {
		if _, err := db.Exec(q, now, old); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	notes := filepath.Join(old, "notes", "plan.md")
	os.MkdirAll(filepath.Dir(notes), 0o700)
	os.WriteFile(notes, []byte("the plan from before\n"), 0o600)
	os.WriteFile(filepath.Join(old, "USER.md"), []byte("the people\n"), 0o600)
	before := listTree(t, old)

	stop = runDaemon(t, r.root)
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if n := supervisorThreads(t, owner, "c1"); n != 1 {
		t.Fatalf("%d supervisor threads; want the one from before only", n)
	}
	if a := sendAndRead(t, owner, th.ID, "r1", "read notes/plan.md"); a != "read: the plan from before" {
		t.Fatalf("the supervisor from before read %q", a)
	}
	var recorded string
	if err := dbOf(t, r.root).QueryRow(`SELECT dir FROM supervisor_dirs WHERE scope = 'c1'`).Scan(&recorded); err != nil || recorded != old {
		t.Fatalf("the scope's directory is %q, %v; want its thread's, %s", recorded, err, old)
	}
	closeThread(t, r.root, th.ID)
	owner.Close()
	stop()

	stop = runDaemon(t, r.root)
	defer stop()
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	next := supervisorThread(t, owner, "c1", control.Idle)
	if next.ID == th.ID || threadDir(t, r.root, next.ID) != old {
		t.Fatalf("the next supervisor thread %d is in %s; want a new one in %s", next.ID, threadDir(t, r.root, next.ID), old)
	}
	if a := sendAndRead(t, owner, next.ID, "r2", "read notes/plan.md"); a != "read: the plan from before" {
		t.Fatalf("the next supervisor read %q", a)
	}
	if after := listTree(t, old); after != before {
		t.Fatalf("the directory changed:\n%s\nwas\n%s", after, before)
	}
	if _, err := os.Lstat(workspace.ScopePath(r.root, "c1")); err == nil {
		t.Fatal("a second directory was made for the scope")
	}
}

// listTree lists what is under dir, each path with its mode, size, time
// and content.
func listTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %v %d %d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %q", c)
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
