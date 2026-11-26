package claudecode

import (
	"encoding/json"
	"fmt"
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

// A closed window is resumed after TakenIn has been cleared. Both the
// origin and directory must survive persisted specs and daemon settings.
func TestLaunchPreservesDefaultConfigOrigin(t *testing.T) {
	for _, daemonOverride := range []bool{false, true} {
		for _, c := range []struct {
			name, recorded, wantDir, wantHome string
			defaultDir                        bool
			legacy                            bool
		}{
			{name: "default", recorded: "/home/alice/.claude", wantHome: "/home/alice", defaultDir: true},
			{name: "explicit default path", recorded: "/home/alice/.claude", wantDir: "/home/alice/.claude", wantHome: "/srv/daemon"},
			{name: "explicit other path", recorded: "/home/alice/work-config", wantDir: "/home/alice/work-config", wantHome: "/srv/daemon"},
			{name: "legacy", recorded: "/home/alice/.claude", wantDir: "/home/alice/.claude", wantHome: "/srv/daemon", legacy: true},
		} {
			t.Run(fmt.Sprintf("%s/daemonOverride=%v", c.name, daemonOverride), func(t *testing.T) {
				a := pluginAdapter(t)
				a.environ = func() []string {
					env := []string{"PATH=/usr/bin", "HOME=/srv/daemon"}
					if daemonOverride {
						env = append(env, "CLAUDE_CONFIG_DIR=/srv/daemon/config", "CLAUDE_CONFIG_DIR=/srv/daemon/duplicate")
					}
					return env
				}
				spec := agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir(), ResumeID: "00000000-0000-4000-8000-000000000001", ConfigDir: c.recorded, ConfigDirDefault: c.defaultDir}
				b, err := json.Marshal(spec)
				if err != nil {
					t.Fatal(err)
				}
				if c.legacy && strings.Contains(string(b), "ConfigDirDefault") {
					t.Fatal("legacy encoding gained a required field")
				}
				spec = agent.Spec{}
				if err := json.Unmarshal(b, &spec); err != nil {
					t.Fatal(err)
				}
				l, err := a.Launch(spec)
				if err != nil {
					t.Fatal(err)
				}
				values := func(key string) []string {
					var out []string
					for _, kv := range l.Env {
						if v, ok := strings.CutPrefix(kv, key+"="); ok {
							out = append(out, v)
						}
					}
					return out
				}
				wantDirs := []string(nil)
				if c.wantDir != "" {
					wantDirs = []string{c.wantDir}
				}
				if got := values("CLAUDE_CONFIG_DIR"); !slices.Equal(got, wantDirs) {
					t.Errorf("config overrides %q, want %q", got, wantDirs)
				}
				if got := values("HOME"); !slices.Equal(got, []string{c.wantHome}) {
					t.Errorf("homes %q, want %q", got, c.wantHome)
				}
			})
		}
	}
}
