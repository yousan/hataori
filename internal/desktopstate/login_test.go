package desktopstate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginRegistrationIsExplicitRootBoundAndRetainedDuringUpdate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, rootFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ReadLogin(ctx); err == nil {
		t.Fatal("implicit login registration")
	}
	one := stagedFixture(t, s, "1.0.0")
	if err := s.Register(ctx, one); err != nil {
		t.Fatal(err)
	}
	dir := rootFixture(t)
	env := map[string]string{"HOME": dir, "PATH": "/usr/bin:/bin"}
	reg, err := s.InstallLogin(ctx, dir, env)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(reg.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"<key>AbandonProcessGroup</key><true/>", "<key>KeepAlive</key><true/>", "<key>ThrottleInterval</key>", "<string>launcher</string>", xmlText(one.Binary), xmlText(s.root.Name())} {
		if !bytes.Contains(before, []byte(required)) {
			t.Fatalf("missing %q", required)
		}
	}
	two := stagedFixture(t, s, "2.0.0")
	if err := s.Select(ctx, two.Version, two.Binary); err != nil {
		t.Fatal(err)
	}
	if again, err := s.InstallLogin(ctx, dir, env); err != nil || again != reg {
		t.Fatalf("update replaced registration: %+v %v", again, err)
	}
	after, _ := os.ReadFile(reg.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("referenced launcher changed")
	}
	got, _ := s.Read(ctx)
	if got.Intent != Stopped {
		t.Fatal("login registration undid Stop")
	}
	if err := os.WriteFile(reg.Path, []byte("other registration"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadLogin(ctx); err == nil {
		t.Fatal("modified registration could authorize service action")
	}
	if data, _ := os.ReadFile(reg.Path); string(data) != "other registration" {
		t.Fatal("modified registration overwritten")
	}
}

func TestLoginRefusesExistingForeignRegistrationAndEnvironment(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, rootFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Register(ctx, stagedFixture(t, s, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	dir := rootFixture(t)
	if _, err := s.InstallLogin(ctx, dir, map[string]string{"HOME": dir, "PATH": "/bin", "BOT_TOKEN": "not copied"}); err == nil {
		t.Fatal("credential environment allowed")
	}
	name := filepath.Join(dir, s.loginLabel()+".plist")
	const original = "existing user's registration"
	os.WriteFile(name, []byte(original), 0o600)
	if _, err := s.InstallLogin(ctx, dir, map[string]string{"HOME": dir, "PATH": "/bin"}); err == nil {
		t.Fatal("foreign registration accepted")
	}
	if data, _ := os.ReadFile(name); string(data) != original {
		t.Fatal("foreign registration changed")
	}
}
