package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
)

// Use the real adapter's launch environment but execute only the fake
// program on pipes. No installed coding agent or tmux is invoked.
type environmentAdapter struct {
	agent.Adapter
	real   agent.Adapter
	record string
}

func (a environmentAdapter) Launch(spec agent.Spec) (agent.Launch, error) {
	l, err := a.real.Launch(spec)
	if err != nil {
		return agent.Launch{}, err
	}
	b, _ := json.Marshal(fake.Config{Command: []string{os.Args[0], "role", "fake"}, Env: l.Env, Record: a.record})
	f, err := fake.New(b)
	if err != nil {
		return agent.Launch{}, err
	}
	return f.Launch(spec)
}

func TestRepositoryEnvironmentReachesBothAdapters(t *testing.T) {
	// Keep adapter configuration and credentials entirely in test directories.
	for _, name := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(name, t.TempDir())
	}
	t.Setenv("GOFLAGS", "daemon-value")
	t.Setenv("LANG", "daemon-lang")
	for _, newAdapter := range []struct {
		name string
		new  func([]byte) (agent.Adapter, error)
	}{{"claude-code", claudecode.New}, {"codex", codex.New}} {
		t.Run(newAdapter.name, func(t *testing.T) {
			real, err := newAdapter.new([]byte(`{"pass_env":["GOFLAGS"]}`))
			if err != nil {
				t.Fatal(err)
			}
			record := t.TempDir()
			f := newFixture(t, options{wrap: func(a agent.Adapter) agent.Adapter {
				return environmentAdapter{Adapter: a, real: real, record: record}
			}})
			cfg, err := instance.ParseConfig([]byte(`{"owner":"x","trigger":"!go","idle":"1m","repositories":[{"url":"file:///unused","env":{"GOFLAGS":"-trimpath","LANG":"repo-lang","EMPTY":""}}]}`))
			if err != nil {
				t.Fatal(err)
			}
			f.run.RepositoryEnv = map[string]map[string]string{cfg.Repositories[0].URL: cfg.Repositories[0].Env}
			f.start("reply A")
			waitFor(t, "A's first turn", 15*time.Second, f.has("turn_ended", 1))
			check := func(want map[string]string, count int) {
				t.Helper()
				files, err := filepath.Glob(filepath.Join(record, "start-*.json"))
				if err != nil || len(files) != count {
					t.Fatalf("recorded starts: %d, %v; want %d", len(files), err, count)
				}
				for _, path := range files {
					b, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var got struct {
						Env []string
						Dir string
					}
					if err := json.Unmarshal(b, &got); err != nil {
						t.Fatal(err)
					}
					if got.Dir != filepath.Join(f.root, "work", "1") {
						continue
					}
					for name, value := range want {
						var values []string
						for _, kv := range got.Env {
							k, v, _ := strings.Cut(kv, "=")
							if k == name {
								values = append(values, v)
							}
						}
						if !slices.Equal(values, []string{value}) {
							t.Fatalf("%s values %q; want exactly %q", name, values, value)
						}
					}
				}
			}
			want := map[string]string{"GOFLAGS": "-trimpath", "LANG": "repo-lang", "EMPTY": ""}
			check(want, 1)
			t.Logf("fake session using %s launch environment: GOFLAGS=-trimpath LANG=repo-lang EMPTY=", newAdapter.name)
			// Loading a changed config does not relaunch a running session.
			f.run.RepositoryEnv = map[string]map[string]string{"file:///unused": {"GOFLAGS": "new-value"}}
			if err := owner(f.core).Send(context.Background(), thread, "again", control.Input{Text: "reply still A"}, control.Refuse); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "A's second turn", 15*time.Second, f.has("turn_ended", 2))
			check(want, 1)
			if slept, err := f.run.Sleep(context.Background(), thread); err != nil || !slept {
				t.Fatalf("sleep: %v, %v", slept, err)
			}
			// Remove the old capture so the resumed process is checked alone.
			files, _ := filepath.Glob(filepath.Join(record, "start-*.json"))
			for _, path := range files {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := owner(f.core).Send(context.Background(), thread, "wake", control.Input{Text: "reply new A"}, control.Refuse); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "A's resumed turn", 15*time.Second, f.has("turn_ended", 3))
			check(map[string]string{"GOFLAGS": "new-value", "LANG": "daemon-lang"}, 1)
			if _, err := owner(f.core).Start(context.Background(), control.StartRequest{Key: "B", Scope: "s", Thread: "B", Repo: "file:///other", Input: control.Input{Text: "reply B"}}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "B's turn", 15*time.Second, f.has("turn_ended", 4))
			files, _ = filepath.Glob(filepath.Join(record, "start-*.json"))
			found := false
			for _, path := range files {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var got struct {
					Env []string
					Dir string
				}
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatal(err)
				}
				if got.Dir == filepath.Join(f.root, "work", "2") {
					found = true
					if !slices.Contains(got.Env, "GOFLAGS=daemon-value") || !slices.Contains(got.Env, "LANG=daemon-lang") || slices.Contains(got.Env, "EMPTY=") {
						t.Errorf("B received repository overrides")
					}
				}
			}
			if !found {
				t.Fatal("B's environment was not recorded")
			}
			var stored string
			if err := f.st.DB.QueryRow(`SELECT spec FROM sessions WHERE id = 1`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored, "new-value") || strings.Contains(stored, "-trimpath") {
				t.Fatal("repository values leaked into stored spec")
			}
		})
	}
}

func TestRepositoryEnvironmentSkipsSupervisor(t *testing.T) {
	f := newFixture(t, options{confines: true})
	f.run.RepositoryEnv = map[string]map[string]string{"": {"SHOULD_NOT_REACH_SUPERVISOR": "yes"}}
	if _, err := f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "supervisor", Scope: "s", Thread: "supervisor", Input: control.Input{Text: "reply confined"}}, control.KindSupervisor, core.InDir(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "supervisor turn", 15*time.Second, f.has("turn_ended", 1))
	b, err := os.ReadFile(filepath.Join(StateDir(f.root, 1), host.SpecFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SHOULD_NOT_REACH_SUPERVISOR") {
		t.Fatal("supervisor received repository environment")
	}
}
