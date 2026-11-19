package runner

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
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

// A new session's codec gets the spec as it was launched: confined when its
// thread was started confined, and with the files its scope trusts at that
// launch (#423).
func TestCodecGetsTheLaunchedSpec(t *testing.T) {
	rec := &specs{of: map[string][]agent.Spec{}}
	f := newFixture(t, options{confines: true, wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.run.TrustFiles = func(scope string) []string {
		if scope == "s" {
			return []string{".claude/settings.json"}
		}
		return nil
	}
	f.start("reply hello")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	if _, err := f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "k2", Scope: "s", Thread: "t2", Input: control.Input{Text: "reply confined"}}); err != nil {
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
