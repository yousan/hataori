package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yousan/hataori/internal/agent"
)

// Hook events the codec reads. Each is registered with the command
// `hataori hook`; the ones in waiting stay open for an answer.
var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"PostToolUseFailure", "PermissionRequest", "SubagentStart", "SubagentStop", "Stop",
	"StopFailure", "PreCompact", "PostCompact", "SessionEnd"}

// waiting are the hooks whose reply decides something, by event and tool
// matcher: a permission request, and a question to the user.
var waiting = []struct{ event, matcher string }{
	{"PermissionRequest", ""},
	{"PreToolUse", "AskUserQuestion"},
}

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds
}

type hookMatcher struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

// settings is the file passed with --settings: a hook for every event the
// codec reads, and the status line (statusline.go). A waiting hook runs `hataori hook --wait` with the configured
// timeout, and Claude Code is told the same timeout. A PreToolUse of
// AskUserQuestion is therefore reported twice, once waiting; the codec
// reads the waiting one.
func (a *Adapter) settings(spec agent.Spec) ([]byte, error) {
	secs := int(a.hookTimeout.Seconds())
	hooks := map[string][]hookMatcher{}
	plain := shellQuote(a.hataori) + " hook"
	for _, ev := range hookEvents {
		wait := false
		for _, w := range waiting {
			if w.event == ev && w.matcher == "" {
				wait = true
			}
		}
		if wait {
			hooks[ev] = []hookMatcher{{Hooks: []hookCmd{{Type: "command", Timeout: secs,
				Command: fmt.Sprintf("%s --wait --timeout %ds", plain, secs)}}}}
		} else {
			hooks[ev] = []hookMatcher{{Hooks: []hookCmd{{Type: "command", Command: plain}}}}
		}
	}
	for _, w := range waiting {
		if w.matcher != "" {
			hooks[w.event] = append(hooks[w.event], hookMatcher{Matcher: w.matcher, Hooks: []hookCmd{{
				Type: "command", Timeout: secs, Command: fmt.Sprintf("%s --wait --timeout %ds", plain, secs)}}})
		}
	}
	// The user's own vim mode would make Escape leave insert mode instead
	// of stopping the turn; settings given with --settings take precedence
	// over the user's global choice, so the session runs in normal mode.
	// The status line hands on the remaining usage, and shows the user's.
	return json.MarshalIndent(map[string]any{"hooks": hooks, "editorMode": "normal",
		"statusLine": a.statusLineSetting(spec.Dir, spec.StateDir)}, "", "  ")
}

// shellQuote quotes s for the shell Claude Code runs a hook command in.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
