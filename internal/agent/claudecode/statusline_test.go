package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// The status line inputs of the research note (#191), Claude Code 2.1.327.
var noteCaptures = filepath.Join("..", "..", "..", "docs", "notes", "agent-usage-limits", "captures")

func noteCapture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(noteCaptures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withConfig makes the adapter's environment a home of its own, so the
// user's real settings are never read; config is its settings.json, if any.
func withConfig(t *testing.T, a *Adapter, config string) {
	t.Helper()
	home := t.TempDir()
	if config != "" {
		os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
		if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a.environ = func() []string { return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")} }
}

func launchStatusLine(t *testing.T, a *Adapter, s agent.Spec) statusLine {
	t.Helper()
	l, err := a.Launch(s)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		StatusLine statusLine `json:"statusLine"`
	}
	if err := json.Unmarshal(l.Files[SettingsFile], &set); err != nil {
		t.Fatal(err)
	}
	return set.StatusLine
}

// The user's status line is the one Claude Code would show without
// hataori's --settings: the project's local settings over its shared ones
// over the user's own. hataori's runs it, keeps its padding, and refreshes
// at least as often as it asked.
func TestStatusLineRunsTheUsersOwn(t *testing.T) {
	a := newAdapter(t, `{"hataori": "/opt/hataori"}`)
	s := spec(t)

	withConfig(t, a, "")
	if sl := launchStatusLine(t, a, s); sl.Type != "command" || strings.Contains(sl.Command, "--run") ||
		sl.Command != "'/opt/hataori' statusline --last '"+filepath.Join(s.StateDir, statusLineLast)+"'" || sl.RefreshInterval != statusLineRefresh {
		t.Fatalf("without a status line of the user's: %+v", sl)
	}

	withConfig(t, a, `{"statusLine": {"type": "command", "command": "echo \"it's $USER\"", "padding": 2, "refreshInterval": 10}}`)
	sl := launchStatusLine(t, a, s)
	if !strings.HasSuffix(sl.Command, ` --run 'echo "it'\''s $USER"'`) || sl.Padding == nil || *sl.Padding != 2 || sl.RefreshInterval != 10 {
		t.Fatalf("with the user's: %+v", sl)
	}

	os.MkdirAll(filepath.Join(s.Dir, ".claude"), 0o700)
	os.WriteFile(filepath.Join(s.Dir, ".claude", "settings.json"), []byte(`{"statusLine": {"type": "command", "command": "echo project"}}`), 0o600)
	if sl := launchStatusLine(t, a, s); !strings.HasSuffix(sl.Command, ` --run 'echo project'`) || sl.Padding != nil || sl.RefreshInterval != statusLineRefresh {
		t.Fatalf("with the project's: %+v", sl)
	}
	os.WriteFile(filepath.Join(s.Dir, ".claude", "settings.local.json"), []byte(`{"statusLine": {"type": "command", "command": "echo local"}}`), 0o600)
	if sl := launchStatusLine(t, a, s); !strings.HasSuffix(sl.Command, ` --run 'echo local'`) {
		t.Fatalf("with the local one: %+v", sl)
	}
}

// The status line's command, run by a shell as Claude Code runs it, prints
// the user's line, and hands rate_limits to the session host once; the
// codec reads them from the host's log as a Limits event. An input without
// rate_limits hands on nothing.
func TestStatusLineHandsOnTheLimits(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { killGroups(dir) })
	if _, err := host.Start(host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, dir,
		host.Spec{Argv: []string{os.Args[0], "role", "idle"}, Env: []string{}}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "hataori")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec '"+os.Args[0]+"' role \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := newAdapter(t, `{"hataori": "`+bin+`"}`)
	withConfig(t, a, `{"statusLine": {"type": "command", "command": "sed -n 's/.*\"display_name\": \"\\([^\"]*\\)\".*/\\1/p'"}}`)
	sl := launchStatusLine(t, a, agent.Spec{Dir: t.TempDir(), StateDir: dir})

	run := func(input []byte) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", sl.Command)
		cmd.Env = []string{host.HookEnv(dir), "PATH=" + os.Getenv("PATH")}
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	calls := func() [][]byte {
		b, _ := os.ReadFile(filepath.Join(dir, host.EventsFile))
		var out [][]byte
		for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			if bytes.Contains(l, []byte(statusLineEvent)) {
				out = append(out, l)
			}
		}
		return out
	}

	if got := run(noteCapture(t, "claude-statusline-first.json")); got != "Sonnet 5.5" {
		t.Fatalf("the user's line: %q", got)
	}
	run([]byte(`{"model": {"display_name": "Sonnet 5.5"}, "rate_limits": null}`))
	if n := len(calls()); n != 0 {
		t.Fatalf("an input without rate_limits handed on %d calls", n)
	}
	with := noteCapture(t, "claude-statusline-with-rate-limits.json")
	if got := run(with); got != "Sonnet 5.5" {
		t.Fatalf("the user's line: %q", got)
	}
	run(with) // the same again, at once: not handed on
	cs := calls()
	if len(cs) != 1 {
		t.Fatalf("%d calls, want 1", len(cs))
	}
	var k call
	json.Unmarshal(cs[0], &k)
	a2 := newTestCodec().Decode(agent.Hook, cs[0])
	if len(a2.Events) != 1 || a2.Events[0].Kind != agent.Limits {
		t.Fatalf("decoded %+v", a2.Events)
	}
	r := a2.Events[0].Limits
	want := agent.LimitWindow{Name: "seven_day", Minutes: 10080, Used: 21, Resets: time.Unix(1795161600, 0)}
	if !r.At.Equal(time.UnixMilli(k.At)) || r.Every != limitsEvery || len(r.Windows) != 1 || r.Windows[0] != want {
		t.Fatalf("report %+v", r)
	}

	// Once limitsEvery has passed, the same value is handed on again.
	last := filepath.Join(dir, statusLineLast)
	var prev lastHandedOn
	b, _ := os.ReadFile(last)
	json.Unmarshal(b, &prev)
	prev.At -= limitsEvery.Milliseconds()
	b, _ = json.Marshal(prev)
	os.WriteFile(last, b, 0o600)
	run(with)
	if n := len(calls()); n != 2 {
		t.Fatalf("after limitsEvery: %d calls, want 2", n)
	}
}

// Without a host, or without a line of the user's, the command still
// prints the user's line, or nothing.
func TestStatusLineWithoutAHost(t *testing.T) {
	var out bytes.Buffer
	in := noteCapture(t, "claude-statusline-with-rate-limits.json")
	if code := StatusLineMain([]string{"--last", filepath.Join(t.TempDir(), "last"), "--run", "cat >/dev/null; echo mine"},
		bytes.NewReader(in), &out, &out, func(string) string { return "" }); code != 0 || out.String() != "mine\n" {
		t.Fatalf("%d %q", code, out.String())
	}
	out.Reset()
	if code := StatusLineMain([]string{"--last", filepath.Join(t.TempDir(), "last")}, bytes.NewReader(in), &out, &out,
		func(string) string { return "" }); code != 0 || out.Len() != 0 {
		t.Fatalf("%d %q", code, out.String())
	}
}

// A user's line that does not end, and leaves a child holding its output,
// is killed with the child at the timeout: the status line still returns.
func TestStatusLineUsersLineThatHangs(t *testing.T) {
	defer func(d time.Duration) { userLineTimeout = d }(userLineTimeout)
	userLineTimeout = 300 * time.Millisecond
	start := time.Now()
	var out bytes.Buffer
	StatusLineMain([]string{"--last", filepath.Join(t.TempDir(), "last"), "--run", "echo partial; sleep 30 & wait"},
		strings.NewReader("{}"), &out, &out, func(string) string { return "" })
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("returned after %v", d)
	}
}

// The windows Claude Code names come first, in their order; one without
// used_percentage is left out; an empty rate_limits is a report with no
// windows.
func TestDecodeLimitsWindows(t *testing.T) {
	r := decodeLimits(json.RawMessage(`{"spend_limit":{"used_percentage":7.6},"seven_day":{"used_percentage":21,"resets_at":1795161600},"five_hour":{"used_percentage":3,"resets_at":1795000000},"odd":{}}`), time.Unix(1, 0))
	if len(r.Windows) != 3 || r.Windows[0].Name != "five_hour" || r.Windows[0].Minutes != 300 || r.Windows[1].Name != "seven_day" ||
		r.Windows[2] != (agent.LimitWindow{Name: "spend_limit", Used: 8}) {
		t.Fatalf("%+v", r.Windows)
	}
	if r := decodeLimits(json.RawMessage(`{}`), time.Unix(1, 0)); r == nil || len(r.Windows) != 0 {
		t.Fatalf("empty: %+v", r)
	}
}
