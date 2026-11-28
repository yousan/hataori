package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/desktopsetup"
)

func TestDesktopSetupDetectsOnlyTrustedGUIExecutableWithoutInvokingIt(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "desktop-detection-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	executable := filepath.Join(dir, "inert-agent")
	if err := os.WriteFile(executable, []byte("never execute this fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "agent-link")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	look := func(name string) (string, error) {
		if name == "codex" {
			return link, nil
		}
		return "", errors.New("not installed in fixture")
	}
	found := desktopAgents(look)
	canonical, _ := filepath.EvalSymlinks(executable)
	if len(found) != 1 || found[0].Name != codex.Name || found[0].Command != canonical {
		t.Fatalf("trusted GUI detection: %+v", found)
	}
	if err := os.Chmod(executable, 0o777); err != nil {
		t.Fatal(err)
	}
	if found := desktopAgents(look); len(found) != 0 {
		t.Fatal("other-user-writable executable adopted")
	}
	os.Chmod(executable, 0o700)
	os.Chmod(dir, 0o777)
	defer os.Chmod(dir, 0o700)
	if found := desktopAgents(look); len(found) != 0 {
		t.Fatal("other-user-writable ancestor adopted")
	}
}

func TestDesktopSetupBuildUsesExistingParserAndNeverEmbedsToken(t *testing.T) {
	in := desktopsetup.Input{Owner: "fixture-owner", OwnerID: "1000000000000000001", BotID: "1000000000000000002", Scope: "1000000000000000003", Token: "fixture-secret", Repository: "https://example.invalid/fixture/repository.git"}
	selected := desktopsetup.Agent{Name: codex.Name, Command: "/fixture/installed-agent"}
	data, err := desktopBuildConfig(in, selected, "private-token")
	if err != nil || desktopValidateConfig(data) != nil {
		t.Fatal("first-run configuration failed the existing parser", err)
	}
	if bytes.Contains(data, []byte(in.Token)) {
		t.Fatal("token embedded in configuration")
	}
	for _, invalid := range []string{"0", "-1", "99999999999999999999", "1\n2"} {
		in.BotID = invalid
		if _, err := desktopBuildConfig(in, selected, "private-token"); err == nil {
			t.Fatal("invalid adapter identifier accepted")
		}
	}
}
