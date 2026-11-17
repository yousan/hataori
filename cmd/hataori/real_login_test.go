package main

import (
	"os"
	"strings"
	"testing"
)

// The check against the real CLI runs the same way however the CLI is
// logged in, and never reads or writes the login: its source names no
// credential, key, account file, config directory, login command or kind of
// login, passes no variable on to the CLI, and switches the login relay off,
// so that no login command can start. This runs in CI; the check itself
// does not.
func TestRealCheckLeavesTheLoginAlone(t *testing.T) {
	b, err := os.ReadFile("real_test.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ToLower(string(b))
	for _, word := range []string{"credential", "api_key", "apikey", "api key", "oauth", "token", "auth",
		"claude_config_dir", ".claude.json", "subscription", "pass_env", "passenv", "loginlaunch", "probe", "keychain",
		"anthropic_"} {
		if strings.Contains(src, word) {
			t.Errorf("real_test.go names %q", word)
		}
	}
	if !strings.Contains(string(b), `cfg["login_relay"] = false`) {
		t.Error("real_test.go does not switch the login relay off")
	}
	// The adapter's section holds only the command: no variable is passed.
	if !strings.Contains(string(b), `map[string]any{"command": wrapper}`) {
		t.Error("the adapter's section in real_test.go holds more than the command")
	}
}
