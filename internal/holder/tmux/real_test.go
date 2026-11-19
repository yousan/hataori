package tmux

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// TestRealClaudeCodePaste starts the real Claude Code in a pane of a tmux
// server of the test's own, pastes a prompt of several lines through the
// holder, presses Enter through it, and checks that UserPromptSubmit
// reports the same text. It needs no login: the hook runs before the CLI
// finds that it is not logged in. It runs only when asked for:
//
//	HATAORI_REAL_CLAUDE=1 [HATAORI_CAPTURE_DIR=<raw dir>] go test -run TestRealClaudeCodePaste -v ./internal/holder/tmux
//
// A throwaway directory is the CLI's HOME and CLAUDE_CONFIG_DIR, so the
// real configuration is never read or written. With HATAORI_CAPTURE_DIR the
// hook calls are kept there; scrub them before anything is committed under
// testdata/.
func TestRealClaudeCodePaste(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run the real CLI")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	s := newServer(t)
	work, err := os.MkdirTemp("", "hxc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	config, dir, hooks := filepath.Join(work, "config"), filepath.Join(work, "project"), filepath.Join(work, "hooks")
	for _, d := range []string{config, dir, hooks} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Onboarding done and the directory trusted, in the throwaway
	// configuration only, so the CLI shows its input box at once.
	state, _ := json.Marshal(map[string]any{"hasCompletedOnboarding": true,
		"projects": map[string]any{dir: map[string]any{"hasTrustDialogAccepted": true}}})
	if err := os.WriteFile(filepath.Join(config, ".claude.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	hook := map[string]any{"type": "command", "command": `cat > "$(mktemp "` + hooks + `/h.XXXXXX")"`}
	hs := map[string]any{}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		hs[ev] = []any{map[string]any{"hooks": []any{hook}}}
	}
	settings, _ := json.Marshal(map[string]any{"hooks": hs})
	sf := filepath.Join(work, "settings.json")
	if err := os.WriteFile(sf, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf("cd %q && exec env -i HOME=%q CLAUDE_CONFIG_DIR=%q PATH=%q TERM=xterm-256color %q --settings %q --model haiku",
		dir, work, config, os.Getenv("PATH"), claude, sf)
	pane, pid := s.pane(command)
	w := s.window(pane, pid)
	h := &Holder{}

	event := func(name string) map[string]any {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			files, _ := filepath.Glob(filepath.Join(hooks, "h.*"))
			for _, f := range files {
				b, _ := os.ReadFile(f)
				var p map[string]any
				if json.Unmarshal(b, &p) == nil && p["hook_event_name"] == name {
					return p
				}
			}
		}
		t.Fatalf("no %s within 60 s; the pane shows:\n%s", name, s.run("capture-pane", "-p", "-t", pane))
		return nil
	}
	event("SessionStart")
	if err := h.CheckTakeIn(t.Context(), w); err != nil {
		t.Fatalf("the CLI's own pane: %v", err)
	}
	time.Sleep(2 * time.Second) // the input box reads keys some time after SessionStart (#178)

	// No tab: the CLI reports a tab as four spaces (#393).
	prompt := "-a leading dash, `backticks`, \"quotes\", $HOME and ${PATH}\n日本語の二行目 <>&|;\nthe last line;"
	if err := h.Input(t.Context(), w, []agent.Part{{Paste: prompt}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Enter}}); err != nil {
		t.Fatal(err)
	}
	p := event("UserPromptSubmit")
	if got := p["prompt"]; got != prompt {
		t.Fatalf("UserPromptSubmit reported %q, want %q", got, prompt)
	}
	if b := s.run("list-buffers", "-F", "#{buffer_name}"); b != "" {
		t.Fatalf("buffers left: %q", b)
	}
	if out := os.Getenv("HATAORI_CAPTURE_DIR"); out != "" {
		dst := filepath.Join(out, "tmux_paste", "hooks")
		os.MkdirAll(dst, 0o700)
		files, _ := filepath.Glob(filepath.Join(hooks, "h.*"))
		for i, f := range files {
			b, _ := os.ReadFile(f)
			var e map[string]any
			json.Unmarshal(b, &e)
			name := fmt.Sprintf("%02d-%s.json", i+1, strings.ReplaceAll(fmt.Sprint(e["hook_event_name"]), "/", ""))
			os.WriteFile(filepath.Join(dst, name), b, 0o600)
		}
		os.WriteFile(filepath.Join(out, "tmux_paste", "prompt.txt"), []byte(prompt), 0o600)
	}
}
