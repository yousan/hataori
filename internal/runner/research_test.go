package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/store"
)

type researchUnattested struct {
	agent.Adapter
	seen agent.Spec
}

func (a *researchUnattested) CanOmitProjectSettings() bool { return true }
func (a *researchUnattested) Launch(s agent.Spec) (agent.Launch, error) {
	a.seen = s
	return agent.Launch{Argv: []string{"fixture-must-not-execute"}, Files: map[string][]byte{"must-not-write": []byte("fixture")}}, nil
}

// Capability preflight is not launch authority. Exercise the actual runner
// gate without a host, CLI, terminal, process lookup or signal.
func TestResearchCapabilityDoesNotReplaceLaunchAttestation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at, no_project_settings) VALUES (1, 'fixture', 'research', 'owner', 'creating', 0, 0, 1)`,
		`INSERT INTO sessions (id, thread_id, agent, state, created_at, updated_at) VALUES (1, 1, 'fixture', 'creating', 0, 0)`,
	} {
		if _, err := st.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{Store: st, Root: root}
	for _, resume := range []string{"", "fixture-conversation"} {
		a := &researchUnattested{}
		state := t.TempDir()
		_, err := r.launch(ctx, 1, "fixture", a, agent.Spec{Dir: t.TempDir(), StateDir: state, ResumeID: resume}, firstInput{})
		if err == nil || !strings.Contains(err.Error(), "cannot start without the repository's settings") {
			t.Fatalf("runner refused incorrectly: %v", err)
		}
		if !a.seen.NoProjectSettings || a.seen.ResumeID != resume {
			t.Fatal("runner lost settings isolation or resume identity")
		}
		entries, err := os.ReadDir(state)
		if err != nil || len(entries) != 0 {
			t.Fatal("unattested launch wrote files or started a host", err)
		}
	}
}
