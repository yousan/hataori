package main

import (
	"os"
	"strings"
	"testing"
)

// The check against the real CLI runs the same way however the CLI is
// logged in, and never reads or writes the login: its source names no
// credential, key, account file, login command or kind of login, names the
// config directory only to find the settings file it compares, passes no
// variable on to the CLI, and switches the login relay off, so that no
// login command can start. This runs in CI; the check itself does not.
func TestRealCheckLeavesTheLoginAlone(t *testing.T) {
	b, err := os.ReadFile("real_test.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ToLower(string(b))
	for _, word := range []string{"credential", "api_key", "apikey", "api key", "oauth", "token", "auth",
		".claude.json", "subscription", "pass_env", "passenv", "loginlaunch", "probe", "keychain",
		"anthropic_"} {
		if strings.Contains(src, word) {
			t.Errorf("real_test.go names %q", word)
		}
	}
	// The config directory is named once: to find the settings file the
	// last step compares. The check passes it nowhere.
	if n := strings.Count(string(b), "CLAUDE_CONFIG_DIR"); n != 1 || !strings.Contains(string(b), `dir := os.Getenv("CLAUDE_CONFIG_DIR")`) {
		t.Errorf("real_test.go names the config directory %d times; want once, read for the settings file", n)
	}
	if !strings.Contains(string(b), `cfg["login_relay"] = false`) {
		t.Error("real_test.go does not switch the login relay off")
	}
	// The adapter's section holds only the command: no variable is passed.
	if !strings.Contains(string(b), `map[string]any{"command": wrapper}`) {
		t.Error("the adapter's section in real_test.go holds more than the command")
	}
}
