package claudecode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Describe launches only. Synthetic settings live in fixture directories;
// no CLI, hook, statusline command or provider is executed.
func TestResearchLaunchOmitsProjectStatusline(t *testing.T) {
	for _, local := range []bool{false, true} {
		name := "shared"
		if local {
			name = "local"
		}
		t.Run(name, func(t *testing.T) {
			a := newAdapter(t, `{"hataori":"/fixture/hataori"}`)
			s := spec(t)
			withConfig(t, a, `{"statusLine":{"type":"command","command":"echo fixture-user-only"}}`)
			dir := filepath.Join(s.Dir, ".claude")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, text := range map[string]string{
				"settings.json":       `{"statusLine":{"type":"command","command":"echo fixture-project-command"}}`,
				"settings.local.json": `{"statusLine":{"type":"command","command":"echo fixture-local-command"}}`,
			} {
				if name == "settings.local.json" && !local {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, resume := range []string{"", "fixture-native-session"} {
				t.Run("resume_"+resume, func(t *testing.T) {
					s.ResumeID, s.NoProjectSettings = resume, true
					sl := launchStatusLine(t, a, s)
					if !strings.Contains(sl.Command, "fixture-user-only") || strings.Contains(sl.Command, "fixture-project-command") || strings.Contains(sl.Command, "fixture-local-command") {
						t.Fatalf("research reintroduced project/local settings: %s", sl.Command)
					}
					l, err := a.Launch(s)
					if err != nil {
						t.Fatal(err)
					}
					if !l.NoProjectSettings || !slices.Contains(l.Argv, "--setting-sources") || !slices.Contains(l.Argv, "user") {
						t.Fatal("research lost its launch attestation or settings-source flag")
					}
					s.NoProjectSettings = false
					ordinaryWant := "fixture-project-command"
					if local {
						ordinaryWant = "fixture-local-command"
					}
					if ordinary := launchStatusLine(t, a, s); !strings.Contains(ordinary.Command, ordinaryWant) || strings.Contains(ordinary.Command, "fixture-user-only") {
						t.Fatalf("ordinary statusline precedence changed: %s", ordinary.Command)
					}
				})
			}
		})
	}
}
