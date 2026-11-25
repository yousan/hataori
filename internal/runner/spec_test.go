package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
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

// mentions is the fake adapter, whose launch does not say NoMentions while
// on is set.
type mentions struct {
	agent.Adapter
	on atomic.Bool
}

func (m *mentions) Launch(s agent.Spec) (agent.Launch, error) {
	l, err := m.Adapter.Launch(s)
	l.NoMentions = l.NoMentions && !m.on.Load()
	return l, err
}

// A confined session is started, for the first time and again, only by an
// adapter whose launch says that its agent takes no input for a mention; an
// ordinary one is started without (#887).
func TestConfinedNeedsNoMentions(t *testing.T) {
	const refusal = "its input could name files to it"
	m := &mentions{}
	f := newFixture(t, options{confines: true, conversations: t.TempDir(), idlePeriod: 300 * time.Millisecond,
		wrap: func(a agent.Adapter) agent.Adapter { m.Adapter = a; return m }})
	confined := func(key string) (control.ThreadID, error) {
		return f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
			control.StartRequest{Key: key, Scope: "s", Thread: "t-" + key, Input: control.Input{Text: "reply confined"}}, control.KindSupervisor, core.InDir(t.TempDir()))
	}
	m.on.Store(true)
	f.start("reply ordinary")
	waitFor(t, "the ordinary turn", 15*time.Second, f.has("turn_ended", 1))
	if _, err := confined("k2"); err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("the start: %v, want it refused", err)
	}
	for _, name := range []string{host.HostPIDFile, host.PIDFile} {
		if _, err := os.Stat(filepath.Join(StateDir(f.root, 2), name)); err == nil {
			t.Errorf("%s exists: a session host or an agent was started", name)
		}
	}

	m.on.Store(false)
	closer := f.core.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all | control.OpClose})
	if err := closer.CloseThread(context.Background(), 2); err != nil { // a scope has one supervisor thread
		t.Fatal(err)
	}
	id, err := confined("k3")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the confined turn", 15*time.Second, f.has("turn_ended", 2))
	waitFor(t, "its sleep", 15*time.Second, func() bool {
		var state string
		if err := f.st.DB.QueryRow(`SELECT state FROM sessions WHERE thread_id = ?`, int64(id)).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state == string(control.Asleep)
	})
	m.on.Store(true)
	if err := owner(f.core).Send(context.Background(), id, "again", control.Input{Text: "reply again"}, control.Refuse); err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("the wake: %v, want it refused", err)
	}
	if n := counts(t, f.st)["turn_ended"]; n != 2 {
		t.Errorf("%d turns ended, want 2: the confined session ran again", n)
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
