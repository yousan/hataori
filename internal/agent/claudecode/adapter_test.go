package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

func newAdapter(t *testing.T, config string) *Adapter {
	t.Helper()
	a, err := New([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Adapter)
}

func spec(t *testing.T) agent.Spec {
	return agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir(), Model: "haiku", Effort: "low", Instructions: "Be brief."}
}

func argAfter(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}

func TestLaunchNewAndResumed(t *testing.T) {
	a := newAdapter(t, `{"hataori": "/opt/hataori/bin/hataori"}`)
	s := spec(t)
	l, err := a.Launch(s)
	if err != nil {
		t.Fatal(err)
	}
	if l.Argv[0] != "claude" || !l.Terminal {
		t.Fatalf("launch: %v terminal=%v", l.Argv, l.Terminal)
	}
	for flag, want := range map[string]string{"--model": "haiku", "--effort": "low", "--permission-mode": "default"} {
		if got, _ := argAfter(l.Argv, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	id, ok := argAfter(l.Argv, "--session-id")
	if !ok || len(id) != 36 || slices.Contains(l.Argv, "--resume") {
		t.Fatalf("new session: %v", l.Argv)
	}
	if f, _ := argAfter(l.Argv, "--append-system-prompt-file"); string(l.Files[filepath.Base(f)]) != "Be brief." {
		t.Fatalf("instructions file %q: %q", f, l.Files[filepath.Base(f)])
	}
	if f, _ := argAfter(l.Argv, "--settings"); l.Files[filepath.Base(f)] == nil {
		t.Fatalf("settings file %q is not among the files to write", f)
	}

	s.ResumeID = "00000000-0000-4000-8000-000000000001"
	l, _ = a.Launch(s)
	if got, _ := argAfter(l.Argv, "--resume"); got != s.ResumeID || slices.Contains(l.Argv, "--session-id") {
		t.Fatalf("resumed session: %v", l.Argv)
	}
}

func TestFilesAreUnderStateDir(t *testing.T) {
	s := spec(t)
	l, err := newAdapter(t, "").Launch(s)
	if err != nil {
		t.Fatal(err)
	}
	for name := range l.Files {
		if !filepath.IsLocal(name) {
			t.Errorf("file %q is not inside the state directory", name)
		}
	}
	for i, arg := range l.Argv {
		if strings.HasPrefix(arg, "/") && i > 0 && !strings.HasPrefix(arg, s.StateDir+"/") {
			t.Errorf("argument %q names a file outside the state directory", arg)
		}
	}
}

func TestSettingsRegisterEveryHook(t *testing.T) {
	l, err := newAdapter(t, `{"hataori": "/opt/hataori's/hataori", "hook_timeout": "1h"}`).Launch(spec(t))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]hookMatcher
	}
	if err := json.Unmarshal(l.Files[SettingsFile], &s); err != nil {
		t.Fatal(err)
	}
	for _, ev := range hookEvents {
		if len(s.Hooks[ev]) == 0 {
			t.Errorf("no hook for %s", ev)
		}
	}
	waits := map[string]bool{}
	for ev, ms := range s.Hooks {
		for _, m := range ms {
			for _, h := range m.Hooks {
				if !strings.HasPrefix(h.Command, `'/opt/hataori'\''s/hataori' hook`) {
					t.Errorf("%s runs %q", ev, h.Command)
				}
				wait := strings.Contains(h.Command, "--wait")
				if wait {
					waits[ev+"/"+m.Matcher] = true
					if h.Timeout != 3600 || !strings.Contains(h.Command, "--timeout 3600s") {
						t.Errorf("%s waits without the configured timeout: %+v", ev, h)
					}
				}
			}
		}
	}
	if len(waits) != 2 || !waits["PermissionRequest/"] || !waits["PreToolUse/AskUserQuestion"] {
		t.Fatalf("waiting hooks: %v", waits)
	}
}

func TestNeverBypassMode(t *testing.T) {
	s := spec(t)
	s.Approvals = agent.AllowAll
	l, _ := newAdapter(t, "").Launch(s)
	all := strings.Join(l.Argv, " ") + string(l.Files[SettingsFile])
	for _, bad := range []string{"--dangerously-skip-permissions", "bypassPermissions", "--allow-dangerously-skip-permissions"} {
		if strings.Contains(all, bad) {
			t.Fatalf("launch uses %s: %v", bad, l.Argv)
		}
	}
}

func TestEnvironmentFromFixedList(t *testing.T) {
	a := newAdapter(t, `{"pass_env": ["ANTHROPIC_API_KEY"]}`)
	a.environ = func() []string {
		return []string{"PATH=/usr/bin", "HOME=/home/user", "GITHUB_TOKEN=secret-1", "DISCORD_TOKEN=secret-2",
			"ANTHROPIC_API_KEY=passed", "TERM=dumb"}
	}
	s := spec(t)
	l, _ := a.Launch(s)
	env := strings.Join(l.Env, "\n")
	for _, bad := range []string{"secret-1", "secret-2", "TERM=dumb"} {
		if strings.Contains(env, bad) {
			t.Errorf("environment holds %s", bad)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/user", "ANTHROPIC_API_KEY=passed", "TERM=xterm-256color", host.HookEnv(s.StateDir)} {
		if !slices.Contains(l.Env, want) {
			t.Errorf("environment lacks %s", want)
		}
	}
}

// fakeCLI writes a script that records its arguments and answers
// --version and auth status as the real CLI does.
func fakeCLI(t *testing.T, status string) (path, log string) {
	dir := t.TempDir()
	path, log = filepath.Join(dir, "claude"), filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" +
		"case \"$*\" in\n" +
		"  --version) echo '2.1.322 (Claude Code)' ;;\n" +
		"  'auth status') cat <<'EOF'\n" + status + "\nEOF\n exit 1 ;;\n" +
		"  *) exit 64 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func TestProbe(t *testing.T) {
	status, _ := os.ReadFile("testdata/2.1.322/login/status-before.json") // a real logged-out answer
	for _, tc := range []struct {
		status   string
		loggedIn bool
	}{
		{string(status), false},
		{`{"loggedIn": true, "authMethod": "claude.ai"}`, true},
	} {
		path, log := fakeCLI(t, tc.status)
		a := newAdapter(t, `{"command": "`+path+`"}`)
		h, err := a.Probe(context.Background())
		if err != nil || !h.Installed || h.Version != "2.1.322" || h.LoggedIn != tc.loggedIn {
			t.Fatalf("probe: %+v, %v", h, err)
		}
		calls, _ := os.ReadFile(log)
		if string(calls) != "--version\nauth status\n" {
			t.Fatalf("probe ran %q; only --version and auth status may run", calls)
		}
	}

	a := newAdapter(t, `{"command": "/nonexistent/claude"}`)
	if h, err := a.Probe(context.Background()); err != nil || h.Installed {
		t.Fatalf("probe of a missing CLI: %+v, %v", h, err)
	}
}
