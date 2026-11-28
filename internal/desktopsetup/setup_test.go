package desktopsetup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "desktop-setup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func TestSetupWritesPrivateNewConfigOnlyAndPreservesExistingLogin(t *testing.T) {
	root := fixture(t)
	build := func(in Input, agent Agent, token string) ([]byte, error) {
		return json.Marshal(map[string]string{"owner": in.Owner, "command": agent.Command, "token_file": token})
	}
	validate := func(data []byte) error { var value map[string]string; return json.Unmarshal(data, &value) }
	s, err := Open(root, []Agent{{Name: "fixture-agent", Command: "/isolated/agent"}}, build, validate)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(Input{Owner: "fixture-owner", Agent: "fixture-agent", Token: "isolated-secret", StartAtLogin: true})
	if err := s.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done:
		t.Fatal("completion published before HTTP success flush")
	default:
	}
	s.Published()
	<-s.Done
	config, err := os.ReadFile(filepath.Join(root, "config/hataori.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]string
	json.Unmarshal(config, &cfg)
	path := filepath.Join(root, "config", cfg["token_file"])
	if token, err := os.ReadFile(path); err != nil || string(token) != "isolated-secret" {
		t.Fatalf("private token was not written: %v", err)
	}
	for _, name := range []string{path, filepath.Join(root, "config/hataori.json")} {
		info, _ := os.Lstat(name)
		if info.Mode().Perm() != 0o600 {
			t.Fatal("new private file has unsafe mode")
		}
	}
	if err := s.Commit(context.Background(), request); err == nil {
		t.Fatal("duplicate uncertain submission replayed")
	}
	s.Close()
	login := filepath.Join(root, "config/existing-agent-login")
	os.WriteFile(login, []byte("retain-login"), 0o600)
	s, err = Open(root, nil, build, validate)
	if err != nil || !s.Info().Existing {
		t.Fatal("existing config was not adopted read-only", err)
	}
	defer s.Close()
	if err := s.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "config/hataori.json"))
	if string(after) != string(config) {
		t.Fatal("existing config was overwritten")
	}
	if data, err := os.ReadFile(login); err != nil || string(data) != "retain-login" {
		t.Fatal("existing login changed")
	}
}

func TestSetupLockAndExplicitConsentRefuseStartup(t *testing.T) {
	root := fixture(t)
	s, err := Open(root, nil, nil, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(root, nil, nil, func([]byte) error { return nil }); err == nil {
		other.Close()
		t.Fatal("two setup/daemon owners admitted")
	}
	if err := s.Commit(context.Background(), []byte(`{"start_at_login":false}`)); err == nil {
		t.Fatal("implicit login-start registration allowed")
	}
	if _, err := os.Stat(filepath.Join(root, "config/hataori.json")); !os.IsNotExist(err) {
		t.Fatal("refused setup wrote configuration")
	}
}
