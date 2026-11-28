package agent_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/agent/fake"
)

// Launch builds descriptions only: no CLI, provider or login is invoked.
func TestSettingsFreeResearchDeclarationAndLaunch(t *testing.T) {
	// Launch descriptions can consult statusline settings. Keep even that
	// read-only lookup in empty fixture homes, never ambient user config.
	for _, key := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	for _, tc := range []struct {
		name string
		new  agent.Factory
		cfg  string
		want bool
	}{
		{"supported", claudecode.New, `{}`, true},
		{"fixture-supported", fake.New, `{"command":["fixture-not-executed"]}`, true},
		{"fixture-refused", fake.New, `{"command":["fixture-not-executed"],"keeps_settings":true}`, false},
		{"codex-ordinary-refused", codex.New, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := tc.new([]byte(tc.cfg))
			if err != nil {
				t.Fatal(err)
			}
			if got := agent.CanOmitProjectSettings(a); got != tc.want {
				t.Fatalf("capability %v, want %v", got, tc.want)
			}
			for _, resume := range []string{"", "fixture-native-session"} {
				state := t.TempDir()
				l, err := a.Launch(agent.Spec{Dir: filepath.Join(state, "work"), StateDir: state, ResumeID: resume, NoProjectSettings: true})
				if err != nil {
					t.Fatal(err)
				}
				if l.NoProjectSettings != tc.want {
					t.Fatalf("resume %q: attestation %v, want %v", resume, l.NoProjectSettings, tc.want)
				}
				if tc.name == "supported" && (!slices.Contains(l.Argv, "--setting-sources") || !slices.Contains(l.Argv, "user")) {
					t.Fatal("supported launch lost settings isolation")
				}
			}
		})
	}
	if agent.CanOmitProjectSettings(nil) {
		t.Fatal("nil adapter advertised research")
	}
}
