package supervisor

import (
	"bytes"
	"slices"
	"testing"

	"github.com/yousan/hataori/internal/core"
)

// What a launch of a scope's supervisor is given names what differs: the
// model and the effort as the section or the scope gives them, hataori's
// layer, the config's, and the MCP server's command.
func TestConfigNamesWhatChanged(t *testing.T) {
	made := func(change func(s *Sessions, sv *Scope)) *Sessions {
		var log bytes.Buffer
		s, _ := layerSessions(t, &log)
		s.Core.Agent = "a"
		s.Core.Scopes = map[string]core.Scope{"c1": {Model: "m-scope", Effort: "low"}}
		s.Core.Agents = map[string]core.AgentDefaults{"b": {Model: "m-b"}}
		sv := s.Scopes["c1"]
		change(s, &sv)
		s.Scopes["c1"] = sv
		return s
	}
	was, ok := made(func(*Sessions, *Scope) {}).Config("c1", "a")
	if !ok || was.Model != "m-scope" || was.Effort != DefaultEffort {
		t.Fatalf("%+v, %v; want the scope's model and the default effort", was, ok)
	}
	for name, tc := range map[string]struct {
		change func(s *Sessions, sv *Scope)
		want   []string
	}{
		"nothing":               {func(*Sessions, *Scope) {}, nil},
		"the section's model":   {func(_ *Sessions, sv *Scope) { sv.Model = "m-2" }, []string{"model"}},
		"the scope's model":     {func(s *Sessions, _ *Scope) { s.Core.Scopes["c1"] = core.Scope{Model: "m-3"} }, []string{"model"}},
		"the effort":            {func(_ *Sessions, sv *Scope) { sv.Effort = "max" }, []string{"effort"}},
		"the scope's effort":    {func(s *Sessions, _ *Scope) { s.Core.Scopes["c1"] = core.Scope{Model: "m-scope", Effort: "max"} }, nil},
		"the instructions":      {func(_ *Sessions, sv *Scope) { sv.Instructions = "Other." }, []string{"the config's instructions"}},
		"the autonomy":          {func(_ *Sessions, sv *Scope) { sv.Autonomy = Act }, []string{"the config's instructions"}},
		"the files":             {func(_ *Sessions, sv *Scope) { sv.Files = []string{"/x"} }, nil},
		"the hataori binary":    {func(s *Sessions, _ *Scope) { s.Hataori = "/elsewhere/hataori" }, []string{"MCP server"}},
		"the model and the MCP": {func(s *Sessions, sv *Scope) { s.Root, sv.Model = "/other", "m-2" }, []string{"model", "MCP server"}},
	} {
		now, _ := made(tc.change).Config("c1", "a")
		if got := now.Changed(was); !slices.Equal(got, tc.want) {
			t.Errorf("%s: changed %q, want %q", name, got, tc.want)
		}
	}
	if c, _ := made(func(_ *Sessions, sv *Scope) { sv.Model = "m-2" }).Config("c1", "b"); c.Model != "m-b" {
		t.Errorf("a session of another agent gets the model %q, which the section names for the default agent; want its own", c.Model)
	}
	if _, ok := made(func(*Sessions, *Scope) {}).Config("c2", "a"); ok {
		t.Error("a scope that names no supervisor has a config")
	}
}

func TestSelectedAgentOwnsSupervisorModelAndEffort(t *testing.T) {
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	s.Core.Agent = "workers"
	s.Core.Agents = map[string]core.AgentDefaults{"selected": {Model: "selected-default"}, "workers": {Model: "worker-default"}}
	sv := s.Scopes["c1"]
	sv.Agent = "selected"
	sv.Model = "selected-explicit"
	sv.Effort = "max"
	s.Scopes["c1"] = sv
	selected, _ := s.Config("c1", "selected")
	if selected.Model != "selected-explicit" || selected.Effort != "max" {
		t.Fatal(selected)
	}
	old, _ := s.Config("c1", "workers")
	if old.Model != "worker-default" || old.Effort == "max" {
		t.Fatal("selected adapter's settings leaked into another agent", old)
	}
}
