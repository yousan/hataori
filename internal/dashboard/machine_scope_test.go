package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/store"
)

// Availability and terminal output are fixtures. Scope filtering and public
// field selection use the actual core, database and dashboard HTTP handlers.
// The private session-input marker is not a worker credential; worker-bound
// credential and attachment checks require the remote integration fixture.
type machineScopeRunner struct {
	core.Runner
}

func (machineScopeRunner) Device(context.Context, int64) string { return "" }
func (machineScopeRunner) Terminal(_ context.Context, id int64, _ string, _ uint64) (<-chan control.TerminalChunk, error) {
	if id != 1 {
		return nil, fmt.Errorf("hidden session reached the runner")
	}
	ch := make(chan control.TerminalChunk, 1)
	ch <- control.TerminalChunk{Run: "public-run", Pos: 6, Reset: true, Cols: 80, Rows: 24, Data: []byte("public")}
	close(ch)
	return ch, nil
}

func TestMachineDashboardScopeAndPrivateFields(t *testing.T) {
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
	if err := st.SyncMachines(ctx, []store.MachineBinding{
		{Key: "public-worker", SSH: "private-destination-marker", Root: "private-root-marker"},
		{Key: "hidden-machine-marker", SSH: "hidden-destination-marker", Root: "hidden-root-marker"},
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	for i, scope := range []string{"visible", "hidden-scope-marker"} {
		id := i + 1
		machine := []string{"public-worker", "hidden-machine-marker"}[i]
		if _, err := st.DB.Exec(`INSERT INTO threads (id,scope,key,title,repo,started_by,state,created_at,updated_at,machine,workspace) VALUES (?,?,?,?,?,'owner','running',?,?,?,?)`,
			id, scope, fmt.Sprint(id), "title-"+scope, "project", at.UnixMilli(), at.UnixMilli(), machine, "private-root-marker"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`INSERT INTO sessions (id,thread_id,agent,state,created_at,updated_at,first_input) VALUES (?,?,'fake','running',?,?,?)`,
			id, id, at.UnixMilli(), at.UnixMilli(), `{"text":"private-session-input-marker"}`); err != nil {
			t.Fatal(err)
		}
	}
	c := &core.Core{
		Store: st, Runner: machineScopeRunner{},
		Scopes: map[string]core.Scope{"visible": {Enabled: true}, "hidden-scope-marker": {Enabled: true}},
		People: people.New(nil, "owner", nil),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Away: &core.MachineAway{Status: func(context.Context, string) (control.MachineAvailability, error) {
			return control.MachineAvailability{State: "away", At: at, Reason: "transport"}, nil
		}},
	}
	bound := c.As(control.Caller{Principal: "owner", Scopes: []string{"visible"}, May: control.OpRead})
	s := start(t, Config{}, func(s *Server) { s.For = only(bound) })
	token := signIn(t, s)
	for _, path := range []string{ThreadsPath, ThreadPath + "?thread=1&recent=0&events=0", ThreadEventsPath + "?thread=1", WorkspacePath + "?thread=1", TerminalPath + "?thread=1", ScopesPath} {
		a := do(t, s, req{path: path, token: token})
		for _, marker := range []string{"private-destination-marker", "private-root-marker", "private-session-input-marker", "hidden-machine-marker", "hidden-scope-marker", "hidden-destination-marker", "hidden-root-marker"} {
			if strings.Contains(a.body, marker) {
				t.Fatalf("%s exposed %s: %s", path, marker, a.body)
			}
		}
		if path == ThreadsPath {
			var got []control.ThreadInfo
			if err := json.Unmarshal([]byte(a.body), &got); a.StatusCode != http.StatusOK || err != nil || len(got) != 1 || got[0].ID != 1 || got[0].Machine != "public-worker" || got[0].Availability.State != "away" {
				t.Fatalf("scoped machine list: %d %s", a.StatusCode, a.body)
			}
		}
	}
	// The same HTTP response for a hidden thread and one that never existed,
	// across each read that names a thread; no hidden runner is reached.
	for _, path := range []string{ThreadPath, ThreadEventsPath, WorkspacePath, TerminalPath} {
		hidden := do(t, s, req{path: path + "?thread=2", token: token})
		missing := do(t, s, req{path: path + "?thread=999", token: token})
		if hidden.StatusCode != http.StatusNotFound || hidden.StatusCode != missing.StatusCode || hidden.body != missing.body {
			t.Fatalf("%s distinguishes hidden and missing threads: %d %q / %d %q", path, hidden.StatusCode, hidden.body, missing.StatusCode, missing.body)
		}
	}
}
