package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoredScopeIdentityAndClaims(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	free := func() (bool, error) { return true, nil }
	sc, made, err := s.KeepScope(ctx, "chat", "opaque:channel", free)
	if err != nil || !made || sc.Key != "opaque:channel" || sc.Created.IsZero() {
		t.Fatalf("%+v %v %v", sc, made, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, made, err := s.KeepScope(ctx, "chat", sc.Channel, func() (bool, error) { t.Fatal("existing identity rechecked a directory"); return false, nil })
	if err != nil || made || again != sc {
		t.Fatalf("%+v %v %v", again, made, err)
	}
	other, made, err := s.KeepScope(ctx, "another", sc.Channel, free)
	if err != nil || made || other.Key != "" {
		t.Fatalf("another client took the key: %+v %v %v", other, made, err)
	}
	if err := s.ReserveScopeKeys(ctx, map[string]string{sc.Key: "another", "new": "chat"}); err == nil || !strings.Contains(err.Error(), "chat") || !strings.Contains(err.Error(), "another") {
		t.Fatal(err)
	}
	var n int
	s.DB.QueryRow(`SELECT count(*) FROM scope_keys WHERE key='new'`).Scan(&n)
	if n != 0 {
		t.Fatal("a failed startup reserved other keys")
	}
	if err := s.ReserveScopeKeys(ctx, map[string]string{"taken": "chat"}); err != nil {
		t.Fatal(err)
	}
	if sc, made, err := s.KeepScope(ctx, "chat", "taken", free); err != nil || made || sc.Key != "" {
		t.Fatalf("configured key reused: %+v %v %v", sc, made, err)
	}
}

func TestStoredScopeDirectoryCollisionAndUnknown(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, newRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc, made, err := s.KeepScope(ctx, "chat", "directory", func() (bool, error) { return false, nil })
	if err != nil || made || sc.Key != "" {
		t.Fatalf("%+v %v %v", sc, made, err)
	}
	sc, made, err = s.KeepScope(ctx, "chat", "directory", func() (bool, error) { t.Fatal("forgot directory tombstone"); return true, nil })
	if err != nil || made || sc.Key != "" {
		t.Fatalf("%+v %v %v", sc, made, err)
	}
	cause := errors.New("cannot tell")
	if _, _, err := s.KeepScope(ctx, "chat", "unknown", func() (bool, error) { return false, cause }); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	var n int
	s.DB.QueryRow(`SELECT count(*) FROM scope_keys WHERE key='unknown'`).Scan(&n)
	if n != 0 {
		t.Fatal("uncertain check changed the store")
	}
}

func TestStoredScopeMigrationReservesHistoricalKeys(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	old, err := open(ctx, filepath.Join(root, File), migrations[:len(migrations)-1])
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.DB.Exec(`INSERT INTO threads(scope,key,title,repo,started_by,state,created_at,updated_at) VALUES ('closed-scope','k','','','owner','closed',0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.DB.Exec(`INSERT INTO supervisor_dirs(scope,dir) VALUES ('directory-only','a-directory')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, key := range []string{"closed-scope", "directory-only"} {
		sc, made, err := s.KeepScope(ctx, "chat", key, func() (bool, error) { t.Fatal("historical key reached directory check"); return true, nil })
		if err != nil || made || sc.Key != "" {
			t.Fatalf("%s %+v %v %v", key, sc, made, err)
		}
	}
}
