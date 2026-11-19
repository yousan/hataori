package core

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// stateDirs is a Workspaces that records the directories State is asked
// about.
type stateDirs struct {
	fakeWorkspaces
	asked *[]string
}

func (w stateDirs) State(ctx context.Context, dir string) (control.WorkspaceState, error) {
	*w.asked = append(*w.asked, dir)
	return w.fakeWorkspaces.State(ctx, dir)
}

// A thread made by the release before thread numbers (store's
// testdata/schema19.db) still finds its workspace where it was made, by
// its number, and is found by its link.
func TestAMigratedThreadFindsItsWorkspace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "state"), 0o700)
	b, err := os.ReadFile(filepath.Join("..", "store", "testdata", "schema19.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, store.File), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// The paths it recorded, moved to this root (store's testdata/README.md).
	db, err := sql.Open("sqlite", filepath.Join(root, store.File))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET spec = replace(spec, '/tmp/hataori-fixture/root', ?)`, root); err != nil {
		t.Fatal(err)
	}
	db.Close()
	old := filepath.Join(root, "workspaces", "fake-surface", "c1", "thread2")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.LinkThreads(ctx, map[string]string{"c1": "fake-surface"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatal(err)
	}
	var asked []string
	c := &Core{Store: st, Machine: &session.Machine{Store: st},
		Scopes:     map[string]Scope{"c1": {Enabled: true, Owner: "fake-surface"}},
		Owners:     map[string]control.ScopeOwner{},
		People:     people.New(nil, "owner", nil),
		Workspaces: stateDirs{asked: &asked}, Runner: &fakeRunner{}, Agent: "fake",
		Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	api := c.As(control.Caller{Principal: "owner", Scopes: []string{"c1"}, May: all})

	if _, err := api.Workspace(ctx, 1); err != nil {
		t.Fatalf("Workspace of thread 1: %v", err)
	}
	if !reflect.DeepEqual(asked, []string{old}) {
		t.Fatalf("the workspace read is %v, want %s", asked, old)
	}
	if dir, err := c.WorkspaceDir(ctx, 1); err != nil || dir != old {
		t.Fatalf("WorkspaceDir: %q, %v", dir, err)
	}
	ts, err := api.Threads(ctx, control.Filter{Link: &control.LinkRef{Client: "fake-surface", Channel: "c1", Thread: "thread2"}})
	if err != nil || len(ts) != 1 || ts[0].ID != 1 || ts[0].Scope != "c1" {
		t.Fatalf("the thread by its link: %+v, %v", ts, err)
	}
	d, err := api.Thread(ctx, 1, 100, 100)
	if err != nil || d.Recent == "" || len(d.Events) != 14 || d.Events[0].Thread != 1 || d.Events[0].Scope != "c1" {
		t.Fatalf("thread 1: %+v, %v", d, err)
	}
}
