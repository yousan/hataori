package claudecode

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func TestLookUpLaunchRefusesWithoutRealAgentEvidence(t *testing.T) {
	// Any fallback into ordinary launch construction would inspect adapter
	// settings/environment. This stub must reject before doing either.
	a := &Adapter{environ: func() []string { t.Fatal("look-up inspected ambient environment"); return nil }}
	spec := agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir()}
	l, err := agent.LaunchLookUp(a, spec)
	if !errors.Is(err, agent.ErrLookUpUnavailable) || !strings.Contains(err.Error(), "not been verified") || !reflect.DeepEqual(l, agent.Launch{}) {
		t.Fatalf("unverified launch: %+v %v", l, err)
	}
	for _, change := range []func(*agent.Spec){
		func(s *agent.Spec) { s.ResumeID = "previous" },
		func(s *agent.Spec) { s.Confine = &agent.Confinement{MCP: []agent.MCPServer{{Name: "other"}}} },
		func(s *agent.Spec) { os.WriteFile(filepath.Join(s.Dir, "CLAUDE.md"), []byte("private"), 0600) },
	} {
		s := agent.Spec{Dir: t.TempDir(), StateDir: t.TempDir()}
		change(&s)
		l, err := a.LaunchLookUp(s)
		if !errors.Is(err, agent.ErrLookUpUnavailable) || strings.Contains(err.Error(), "not been verified") || !reflect.DeepEqual(l, agent.Launch{}) {
			t.Fatalf("invalid spec reached profile: %+v %v", l, err)
		}
	}
}
