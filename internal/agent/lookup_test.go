package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type ordinaryOnly struct{ called bool }

func (a *ordinaryOnly) Probe(context.Context, Spec) (Health, error) { return Health{}, nil }
func (a *ordinaryOnly) Launch(Spec) (Launch, error) {
	a.called = true
	return Launch{Argv: []string{"must-not-run"}}, nil
}
func (a *ordinaryOnly) NewCodec(Spec) Codec { return nil }

type lookupAdapter struct {
	ordinaryOnly
	launch  Launch
	lookups int
}

func (a *lookupAdapter) LaunchLookUp(Spec) (Launch, error) { a.lookups++; return a.launch, nil }
func TestLookUpRefusesOrdinaryFallbackAndUnverifiedLaunch(t *testing.T) {
	spec := Spec{Dir: t.TempDir()}
	a := &ordinaryOnly{}
	l, err := LaunchLookUp(a, spec)
	if !errors.Is(err, ErrLookUpUnavailable) || a.called || !reflect.DeepEqual(l, Launch{}) {
		t.Fatalf("fell back: %+v %v", l, err)
	}
	b := &lookupAdapter{launch: Launch{Argv: []string{"must-not-run"}, Confined: true, NoWriteTools: true}}
	l, err = LaunchLookUp(b, spec)
	if !errors.Is(err, ErrLookUpUnavailable) || b.called || b.lookups != 1 || !reflect.DeepEqual(l, Launch{}) {
		t.Fatalf("accepted unverified confinement: %+v %v", l, err)
	}
}
func TestLookUpSpecRejectsContextAndNonemptyDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*Spec)
	}{
		{"resume", "resume", func(s *Spec) { s.ResumeID = "earlier" }},
		{"taken-in", "resume", func(s *Spec) { s.TakenIn = true }},
		{"MCP", "MCP", func(s *Spec) {
			s.Confine = &Confinement{MCP: []MCPServer{{Name: "server", Argv: []string{"must-not-run"}}}}
		}},
		{"layers", "layers", func(s *Spec) { s.Instructions = "private context" }},
		{"configuration", "configuration", func(s *Spec) { s.ConfigDir = "user-settings" }},
		{"trust", "trusts", func(s *Spec) { s.TrustFiles = []string{"project-file"} }},
		{"hooks", "hooks", func(s *Spec) { s.Approvals = AskInChat }},
		{"relative", "absolute", func(s *Spec) { s.Dir = "relative" }},
		{"file", "not empty", func(s *Spec) { os.WriteFile(filepath.Join(s.Dir, "CLAUDE.md"), []byte("context"), 0600) }},
		{"hidden", "not empty", func(s *Spec) { os.WriteFile(filepath.Join(s.Dir, ".settings"), []byte("context"), 0600) }},
		{"link", "link", func(s *Spec) { p := filepath.Join(t.TempDir(), "linked"); os.Symlink(s.Dir, p); s.Dir = p }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{Dir: t.TempDir()}
			tc.change(&spec)
			a := &lookupAdapter{}
			_, err := LaunchLookUp(a, spec)
			if !errors.Is(err, ErrLookUpUnavailable) || !strings.Contains(err.Error(), tc.reason) || a.lookups != 0 || a.called {
				t.Fatalf("context reached adapter: %v", err)
			}
		})
	}
}
func TestLookUpRequiresCompleteAdapterAttestation(t *testing.T) {
	verified := Launch{Argv: []string{"fake-search"}, WebSearchOnly: true, Confined: true, NoMentions: true, NoCommands: true, NoWriteTools: true, NoProjectSettings: true}
	for _, tc := range []struct {
		name   string
		change func(*Launch)
	}{
		{"search", func(l *Launch) { l.WebSearchOnly = false }},
		{"confinement", func(l *Launch) { l.Confined = false }},
		{"mentions", func(l *Launch) { l.NoMentions = false }},
		{"commands", func(l *Launch) { l.NoCommands = false }},
		{"writes", func(l *Launch) { l.NoWriteTools = false }},
		{"settings", func(l *Launch) { l.NoProjectSettings = false }},
		{"files", func(l *Launch) { l.Files = map[string][]byte{"settings": []byte("context")} }},
		{"command", func(l *Launch) { l.Argv = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := verified
			tc.change(&l)
			a := &lookupAdapter{launch: l}
			got, err := LaunchLookUp(a, Spec{Dir: t.TempDir()})
			if !errors.Is(err, ErrLookUpUnavailable) || !reflect.DeepEqual(got, Launch{}) {
				t.Fatalf("accepted incomplete profile %+v: %v", got, err)
			}
		})
	}
	a := &lookupAdapter{launch: verified}
	if got, err := LaunchLookUp(a, Spec{Dir: t.TempDir()}); err != nil || !reflect.DeepEqual(got, verified) {
		t.Fatalf("fake contract: %+v %v", got, err)
	}
}
