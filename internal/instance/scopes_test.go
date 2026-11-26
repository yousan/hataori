package instance

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/workspace"
)

func scopeStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}

func TestStoredScopesRestoreConfiguredAndUnlisted(t *testing.T) {
	ctx := context.Background()
	st, root := scopeStore(t)
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	saved, made, err := KeepScope(ctx, st, root, "chat", "c-new", logger)
	if err != nil || !made {
		t.Fatalf("%+v %v %v", saved, made, err)
	}
	fresh := func() *Config {
		c, err := ParseConfig([]byte(validConfig))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := fresh()
	if err := c.RestoreStoredScopes(ctx, st); err != nil {
		t.Fatal(err)
	}
	got := c.Scopes[len(c.Scopes)-1]
	want := StoredScopePolicy(saved)
	if !reflect.DeepEqual(got, want) || len(c.People.Of(saved.Key)) != 1 || got.Supervisor.Autonomy != AutonomyWatch || got.Supervisor.TurnsPerHour != 6 || got.Supervisor.SendsPerHour != 5 || got.Supervisor.TokensPerDay != 500000 || got.Supervisor.RemindersMax != 3 {
		t.Fatalf("policy %+v %+v", got, got.Supervisor)
	}
	// Configured policy wins on the same identity, including enabled:false.
	c = fresh()
	configured := Scope{Key: saved.Key, Client: saved.Client, Enabled: false, Name: "listed", Supervisor: &Supervisor{Autonomy: AutonomyAct}}
	c.Scopes = append(c.Scopes, configured)
	if err := c.RestoreStoredScopes(ctx, st); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Scopes[len(c.Scopes)-1], configured) {
		t.Fatal("stored defaults overrode configuration")
	}
	c = fresh()
	if err := c.RestoreStoredScopes(ctx, st); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Scopes[len(c.Scopes)-1], want) {
		t.Fatal("unlisted policy not restored")
	}
	c = fresh()
	c.Scopes = append(c.Scopes, Scope{Key: saved.Key, Client: "other"})
	if err := c.RestoreStoredScopes(ctx, st); err == nil || !strings.Contains(err.Error(), "chat") || !strings.Contains(err.Error(), "other") {
		t.Fatal(err)
	}
}

func TestStoredScopeLeavesExistingDirectoryUntouched(t *testing.T) {
	ctx := context.Background()
	st, root := scopeStore(t)
	dir := workspace.ScopePath(root, "only-directory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "work.txt")
	if err := os.WriteFile(file, []byte("uncommitted work"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	sc, made, err := KeepScope(ctx, st, root, "chat", "only-directory", log)
	if err != nil || made || sc.Key != "" {
		t.Fatalf("%+v %v %v", sc, made, err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "uncommitted work" {
		t.Fatal("existing work changed", err)
	}
	if strings.Count(out.String(), "stored scope not created") != 1 {
		t.Fatal(out.String())
	}
}
