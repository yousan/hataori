package supervisor

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/runner"
)

func TestResearchCapabilityInstructionsRefreshWithWorkerAdapter(t *testing.T) {
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	svThread(t, s.Core)
	s.Core.Agent = "blocked"
	s.Core.Agents = map[string]core.AgentDefaults{"blocked": {}, "supported": {}, "supervisor-only": {}}
	s.Core.Scopes = map[string]core.Scope{"c1": {Agent: "blocked"}}
	s.Core.CanOmitProjectSettings = func(name string) bool { return name == "supported" }
	sv := s.Scopes["c1"]
	sv.Agent, sv.Autonomy = "supervisor-only", Act
	// A stale claim on the scope copy cannot override adapter metadata.
	sv.WorkerResearch = ResearchCapability{Agent: "supported", Supported: true}
	s.Scopes["c1"] = sv
	launch := runner.ConfinedLaunch{Session: 1, Thread: 1, Scope: "c1", StateDir: t.TempDir(), Dir: t.TempDir()}
	was, _ := s.Config("c1", "supervisor-only")
	first, err := s.Confine(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ordinary-worker adapter "blocked" cannot omit`, "not a missing repository", "existing worker evidence attributed to that worker", "bounded read-only check"} {
		if !strings.Contains(first.Instructions, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(first.Instructions, "; when a person needs the check, start a `research` thread for it") {
		t.Fatal("unsupported worker was advertised as the forge fallback")
	}
	s.Core.Scopes["c1"] = core.Scope{Agent: "supported"}
	now, _ := s.Config("c1", "supervisor-only")
	if !slices.Contains(now.Changed(was), "the config's instructions") {
		t.Fatal("worker selection did not refresh instructions")
	}
	second, err := s.Confine(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Instructions, `ordinary-worker adapter "supported" supports omitting`) || !strings.Contains(second.Instructions, "; when a person needs the check, start a `research` thread for it") {
		t.Fatal("supported worker was not offered as an available route")
	}
	// A changed adapter configuration is visible even if its name is unchanged.
	s.Core.CanOmitProjectSettings = func(string) bool { return false }
	changed, _ := s.Config("c1", "supervisor-only")
	if !slices.Contains(changed.Changed(now), "the config's instructions") {
		t.Fatal("capability change did not refresh instructions")
	}
}

func TestResearchToolInformationRequiresLoadedCapability(t *testing.T) {
	for _, tool := range tools {
		if tool.Name != ToolStart {
			continue
		}
		research := tool.InputSchema["properties"].(map[string]any)["research"].(map[string]any)["description"].(string)
		for _, want := range []string{"loaded configuration", "selected ordinary-worker adapter", "refused before a worker is created", "Confined supervisor support does not establish"} {
			if !strings.Contains(research, want) {
				t.Fatalf("research tool info lacks %q", want)
			}
		}
		return
	}
	t.Fatal("start tool missing")
}
