package runner

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// specs is the fake adapter with the spec of every codec it makes
// recorded, by session state directory.
type specs struct {
	agent.Adapter
	mu sync.Mutex
	of map[string][]agent.Spec
}

func (s *specs) NewCodec(spec agent.Spec) agent.Codec {
	s.mu.Lock()
	s.of[spec.StateDir] = append(s.of[spec.StateDir], spec)
	s.mu.Unlock()
	return s.Adapter.NewCodec(spec)
}

func (s *specs) first(root string, id int64) (agent.Spec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.of[StateDir(root, id)]
	if len(l) == 0 {
		return agent.Spec{}, false
	}
	return l[0], true
}

// What a confined session's codec knows of its MCP servers is the names of
// those that report, also once a daemon that started again rebuilt it from
// the stored spec; the servers, with their credentials, are not stored
// (#704).
func TestCodecKnowsWhichMCPServersReport(t *testing.T) {
	rec := &specs{of: map[string][]agent.Spec{}}
	f := newFixture(t, options{confines: true, wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.run.Confine = func(context.Context, ConfinedLaunch) (Confined, error) {
		return Confined{MCP: []agent.MCPServer{{Name: "tools", Argv: []string{"x"}, Env: []string{"TOKEN=s3cret"}, Reports: true}, {Name: "other", Argv: []string{"y"}}}}, nil
	}
	if _, err := f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "k", Scope: "s", Thread: "t", Input: control.Input{Text: "reply confined"}}, control.KindSupervisor, core.InDir(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn", 15*time.Second, f.has("turn_ended", 1))
	var stored string
	if err := f.st.DB.QueryRow(`SELECT spec FROM sessions WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var again storedSpec
	if err := json.Unmarshal([]byte(stored), &again); err != nil {
		t.Fatal(err)
	}
	first, _ := rec.first(f.root, 1)
	for name, s := range map[string]agent.Spec{"as launched": first, "as stored": again.Spec} {
		if s.Confine == nil || !slices.Equal(s.Confine.Awaits, []string{"tools"}) || len(s.Confine.MCP) != 0 {
			t.Errorf("%s: %+v", name, s.Confine)
		}
	}
	if strings.Contains(stored, "s3cret") {
		t.Errorf("the stored spec has the server's credential: %s", stored)
	}
}

// A new session's codec gets the spec as it was launched: confined when its
// thread was started confined, and with the files its scope trusts at that
// launch (#423).
func TestCodecGetsTheLaunchedSpec(t *testing.T) {
	rec := &specs{of: map[string][]agent.Spec{}}
	f := newFixture(t, options{confines: true, wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.run.TrustFiles = func(_ context.Context, scope, _ string) []string {
		if scope == "s" {
			return []string{".claude/settings.json"}
		}
		return nil
	}
	f.start("reply hello")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	if _, err := f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "k2", Scope: "s", Thread: "t2", Input: control.Input{Text: "reply confined"}}, control.KindSupervisor, core.InDir(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the confined turn", 15*time.Second, f.has("turn_ended", 2))
	ordinary, ok := rec.first(f.root, 1)
	if !ok || ordinary.Confine != nil || !slices.Equal(ordinary.TrustFiles, []string{".claude/settings.json"}) {
		t.Fatalf("the ordinary session's codec spec: %+v", ordinary)
	}
	confined, ok := rec.first(f.root, 2)
	if !ok || confined.Confine == nil {
		t.Fatalf("the confined session's codec spec has no confinement: %+v", confined)
	}
}
