package desktopstate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func rootFixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "desktop-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func TestStoppedSurvivesSelectionAndReopen(t *testing.T) {
	ctx := context.Background()
	root := rootFixture(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	one := stagedFixture(t, s, "1.0.0")
	one.Intent = Running
	if err := s.Register(ctx, one); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntent(ctx, Stopped); err != nil {
		t.Fatal(err)
	}
	two := stagedFixture(t, s, "2.0.0")
	if err := s.Select(ctx, two.Version, two.Binary); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Read(ctx)
	if err != nil || got.Intent != Stopped || got.Version != "2.0.0" {
		t.Fatalf("stop was undone: %+v %v", got, err)
	}
	if err := s.Register(ctx, Selection{Intent: Running, Version: "2.0.0", Binary: got.Binary}); err == nil {
		t.Fatal("app restart re-registered and overwrote persisted intent")
	}
	if err := s.SetIntent(ctx, Running); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Read(ctx)
	if got.Intent != Running {
		t.Fatal("explicit Start was not persisted")
	}
}

func TestLauncherLeaseIsSeparateAndPermanent(t *testing.T) {
	root := rootFixture(t)
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lease, err := s.LockLauncher()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := s.LockLauncher(); err == nil {
		second.Close()
		t.Fatal("second launcher admitted")
	}
	lease.Close()
	if _, err := os.Stat(filepath.Join(root, "state/lock")); !os.IsNotExist(err) {
		t.Fatal("launcher touched the daemon lock")
	}
	if _, err := os.Stat(filepath.Join(root, "state/desktop/launcher.lock")); err != nil {
		t.Fatal("launcher lease was removed", err)
	}
}

func TestStateRefusesLinksAndNameCollisions(t *testing.T) {
	for _, name := range []string{"STATE", "stAte", "e\u0301", "\u00e9"} {
		t.Run(name, func(t *testing.T) {
			root := rootFixture(t)
			if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
				t.Fatal(err)
			}
			wanted := "state"
			if name == "e\u0301" {
				wanted = "\u00e9"
			} else if name == "\u00e9" {
				wanted = "e\u0301"
			}
			if err := RefuseCollision(root, wanted); err == nil {
				t.Fatal("ambiguous case/Unicode name accepted")
			}
		})
	}
	for _, name := range []string{"launcher.sqlite", "launcher.sqlite-journal"} {
		t.Run(name, func(t *testing.T) {
			root := rootFixture(t)
			path := filepath.Join(root, "state/desktop")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(root, "only-copy.txt")
			os.WriteFile(outside, []byte("retain this work"), 0o600)
			if err := os.Symlink(outside, filepath.Join(path, name)); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(context.Background(), root); err == nil {
				s.Close()
				t.Fatal("linked state was accepted")
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "retain this work" {
				t.Fatalf("outside work changed: %q %v", data, err)
			}
		})
	}
}

func TestUnrecognizedDesktopStateIsPreserved(t *testing.T) {
	root := rootFixture(t)
	dir := filepath.Join(root, "state/desktop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "launcher.sqlite")
	const original = "unrecognized existing work must remain byte-identical"
	if err := os.WriteFile(name, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), root); err == nil {
		s.Close()
		t.Fatal("unknown state accepted")
	}
	data, err := os.ReadFile(name)
	if err != nil || string(data) != original {
		t.Fatalf("existing work changed: %q %v", data, err)
	}
}
