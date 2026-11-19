package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
)

// TestStatusLineCommandRunsHere runs the status line command that the
// Claude Code adapter gives a session, as the CLI would: in a test, its
// hataori binary is this test binary. It must print the session's own
// status line and exit, not run the test suite.
func TestStatusLineCommandRunsHere(t *testing.T) {
	dir, state := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := `{"statusLine": {"type": "command", "command": "echo the session line"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := claudecode.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Launch(agent.Spec{Dir: dir, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal(l.Files[claudecode.SettingsFile], &settings); err != nil || settings.StatusLine.Command == "" {
		t.Fatalf("the launch's status line: %q, %v", settings.StatusLine.Command, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", settings.StatusLine.Command)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("{}")
	// TestMain exits instead of running the suite under this variable.
	cmd.Env = append(os.Environ(), statusLineChildEnv+"=1")
	out, err := cmd.Output()
	if err != nil || string(out) != "the session line\n" {
		t.Fatalf("%s printed %q (%v), want the session's own line", settings.StatusLine.Command, truncate(string(out), 300), err)
	}
}

// statusLineChildEnv marks the status line command this test runs.
const statusLineChildEnv = "HATAORI_STATUSLINE_CHILD"

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
