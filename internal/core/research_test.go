package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

type researchOwner struct {
	fakeOwner
	calls int
}

func (o *researchOwner) NewThread(ctx context.Context, scope, key, title string) (control.LinkRef, error) {
	o.calls++
	return o.fakeOwner.NewThread(ctx, scope, key, title)
}

type researchWorkspaces struct {
	fakeWorkspaces
	calls int
}

func (w *researchWorkspaces) Make(ctx context.Context, id control.ThreadID, repo string, placed func(Placement) error) (string, error) {
	w.calls++
	return w.fakeWorkspaces.Make(ctx, id, repo, placed)
}

// Snapshot every fixture table, so refusal cannot hide a proposal, outbox,
// placement, session, event or key reservation behind a zero thread count.
func researchCounts(t *testing.T, c *Core) map[string]int {
	t.Helper()
	rows, err := c.Store.DB.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, table := range tables {
		out[table] = count(t, c.Store, table)
	}
	return out
}

func TestResearchUnsupportedPreflightHasNoArtifacts(t *testing.T) {
	for _, propose := range []bool{false, true} {
		for _, declared := range []bool{false, true} {
			t.Run(fmt.Sprintf("propose_%v/declaration_%v", propose, declared), func(t *testing.T) {
				f := newFixture(t, fakeOwner{key: "new"})
				var asksIn control.ThreadID
				if propose {
					asksIn, _ = f.running(t, "existing-asker")
				}
				o := &researchOwner{fakeOwner: fakeOwner{key: "new"}}
				w := &researchWorkspaces{}
				f.core.Owners["chat"], f.core.Workspaces = o, w
				if declared {
					f.core.CanOmitProjectSettings = func(string) bool { return false }
				}
				before := researchCounts(t, f.core)
				beforeCalls := f.runner.Calls()
				api := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, Research: true, Propose: propose, AsksIn: asksIn})
				id, err := api.Start(context.Background(), control.StartRequest{Key: "unsupported", Scope: "s", Repo: "https://example.test/present/repo", Research: true, Input: control.Input{Text: "read current checks"}})
				if id != 0 || !errors.Is(err, session.ErrRefused) || !strings.Contains(err.Error(), "settings-free research is unsupported by agent \"fake\"") || !strings.Contains(err.Error(), "cannot omit the repository's project settings") {
					t.Errorf("id %d, refusal %v", id, err)
				}
				after := researchCounts(t, f.core)
				if !reflect.DeepEqual(before, after) || o.calls != 0 || w.calls != 0 || f.runner.Calls() != beforeCalls {
					t.Fatalf("unsupported research created artifacts or consumed an owner/workspace/runner call (proposals %d -> %d)", before["proposals"], after["proposals"])
				}
			})
		}
	}
}

func TestResearchPreflightUsesSelectedAdapter(t *testing.T) {
	f := newFixture(t, fakeOwner{key: "new"})
	f.core.Agents = map[string]AgentDefaults{"fake": {}, "supported": {}}
	f.core.CanOmitProjectSettings = func(name string) bool { return name == "supported" }
	sc := f.core.Scopes["s"]
	sc.Agent = "supported"
	f.core.Scopes["s"] = sc
	name, ok := f.core.ResearchCapability("s")
	if name != "supported" || !ok {
		t.Fatal(name, ok)
	}
	api := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all, Research: true})
	r := control.StartRequest{Key: "supported", Scope: "s", Thread: "existing-chat", Repo: "https://example.test/present/repo", Research: true, Input: control.Input{Text: "look it up"}}
	id, err := api.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var selected string
	var research bool
	if err := f.core.Store.DB.QueryRow(`SELECT s.agent, t.no_project_settings FROM threads t JOIN sessions s ON s.thread_id = t.id WHERE t.id = ?`, int64(id)).Scan(&selected, &research); err != nil {
		t.Fatal(err)
	}
	if selected != "supported" || !research || f.runner.Calls() != "start look it up" {
		t.Fatalf("selected=%s research=%v calls=%s", selected, research, f.runner.Calls())
	}
	before := researchCounts(t, f.core)
	r.Key, r.Agent = "explicit-unsupported", "fake"
	if _, err := api.Start(context.Background(), r); !errors.Is(err, session.ErrRefused) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, researchCounts(t, f.core)) {
		t.Fatal("explicit unsupported override created artifacts")
	}
	// Existing ordinary starts do not require this capability.
	r.Key, r.Thread, r.Research = "ordinary", "ordinary-chat", false
	if _, err := api.Start(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
