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
	// The supervisor's check uses the phase 1 check's helpers, so it names
	// the config directory nowhere itself.
	for file, configDir := range map[string]int{"real_test.go": 1, "real_supervisor_test.go": 0, "real_codex_test.go": 0} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ToLower(string(b))
		for _, word := range []string{"credential", "api_key", "apikey", "api key", "oauth", "token", "auth",
			".claude.json", "subscription", "pass_env", "passenv", "loginlaunch", "probe", "keychain",
			"anthropic_"} {
			if strings.Contains(src, word) {
				t.Errorf("%s names %q", file, word)
			}
		}
		// The config directory is named once: to find the settings file the
		// last step compares. The check passes it nowhere.
		if n := strings.Count(string(b), "CLAUDE_CONFIG_DIR"); n != configDir ||
			configDir > 0 && !strings.Contains(string(b), `dir := os.Getenv("CLAUDE_CONFIG_DIR")`) {
			t.Errorf("%s names the config directory %d times; want %d, read for the settings file", file, n, configDir)
		}
		if file == "real_codex_test.go" {
			if strings.Count(string(b), "CODEX_HOME") != 1 || !strings.Contains(string(b), `dir := os.Getenv("CODEX_HOME")`) || !strings.Contains(string(b), `filepath.Join(dir, "config.toml")`) {
				t.Errorf("%s must name CODEX_HOME only to locate config.toml", file)
			}
			if strings.Contains(string(b), `"-c"`) || strings.Contains(string(b), "'-c'") {
				t.Errorf("%s passes an adapter flag", file)
			}
		}
		if !strings.Contains(string(b), `cfg["login_relay"] = false`) {
			t.Errorf("%s does not switch the login relay off", file)
		}
		// The adapter's section holds only the command: no variable is passed.
		if !strings.Contains(string(b), `map[string]any{"command": wrapper}`) {
			t.Errorf("the adapter's section in %s holds more than the command", file)
		}
	}
}

// The failure dump keeps the text after an OSC ended by ST, not only BEL.
func TestPlainTextEndsOSCAtST(t *testing.T) {
	got := string(plainText([]byte("a\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\ b\x1b[1mc\x07")))
	if got != "a link  b c\x07" {
		t.Fatalf("plain text %q", got)
	}
}
