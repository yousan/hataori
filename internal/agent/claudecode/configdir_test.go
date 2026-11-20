package claudecode

import (
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// A session resumed after its window closed runs with the configuration
// directory the window used, in place of the daemon's.
func TestLaunchWithTheWindowsConfigDir(t *testing.T) {
	a := pluginAdapter(t)
	a.environ = func() []string { return []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/srv/daemon/.claude"} }
	spec := agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir(), ResumeID: "00000000-0000-4000-8000-000000000001"}
	dirs := func(env []string) []string {
		var out []string
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
				out = append(out, v)
			}
		}
		return out
	}
	l, err := a.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirs(l.Env); !slices.Equal(got, []string{"/srv/daemon/.claude"}) {
		t.Fatalf("without one: %q", got)
	}
	spec.ConfigDir = "/home/alice/.claude-work"
	if l, err = a.Launch(spec); err != nil {
		t.Fatal(err)
	}
	if got := dirs(l.Env); !slices.Equal(got, []string{"/home/alice/.claude-work"}) {
		t.Fatalf("with the window's: %q", got)
	}
}
