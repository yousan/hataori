package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	fakeagent "github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// A stored identity crosses daemon restarts and listed/unlisted
// configuration without replacing its conversation or directory.
// This uses only the fake surface/agent and test-owned process ids.
func TestStoredScopeDaemonPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newChatRoot(t, func(cfg map[string]any) {
		adapters := cfg["adapters"].(map[string]any)
		agent := adapters[fakeagent.Name].(fakeagent.Config)
		agent.Confines = true
		adapters[fakeagent.Name] = agent
	})
	r.deadline = time.Now().Add(90 * time.Second)
	if err := os.MkdirAll(filepath.Join(r.root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, r.root)
	if err != nil {
		t.Fatal(err)
	}
	saved, made, err := instance.KeepScope(ctx, st, r.root, fakeSurface, "c-stored", nil)
	if err != nil || !made {
		t.Fatalf("%+v %v %v", saved, made, err)
	}
	st.Close()
	sessions := supervisors(t)
	stop := runDaemon(t, r.root)
	r.connected(t)
	s := sessions.Load()
	if s == nil || !s.Scopes[saved.Key].ConversationsOnly || s.Scopes[saved.Key].Autonomy != supervisor.Watch {
		t.Fatal("fixed policy not wired")
	}
	if got := s.Budgets[saved.Key].Limits; got.Turns != 6 || got.Sends != 5 || got.Tokens != 500000 || s.Reminders[saved.Key].Max != 3 {
		t.Fatalf("small limits: %+v", got)
	}
	id, err := s.Converse(ctx, supervisor.Call{Scope: saved.Key, Thread: "existing-chat", Key: "call", From: fakeSurface + ":" + e2eOwner, Text: "reply stored answer"})
	if err != nil {
		t.Fatal(err)
	}
	api, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	var detail control.ThreadDetail
	eventually(t, "the stored conversation's turn", r.within(20*time.Second), func() bool {
		var e error
		detail, e = api.Thread(ctx, id, 0, control.MaxEvents)
		if e != nil || detail.State != control.Idle {
			return false
		}
		for _, event := range detail.Events {
			if event.Kind == "turn_ended" {
				return true
			}
		}
		return false
	})
	dir, err := s.Dir(ctx, saved.Key)
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("no workspace")
	}
	marker := filepath.Join(dir, "USER.md")
	if err := os.WriteFile(marker, []byte("retained notes"), 0600); err != nil {
		t.Fatal(err)
	}
	api.Close()
	stop()
	path := filepath.Join(r.root, instance.ConfigFile)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(original, &cfg); err != nil {
		t.Fatal(err)
	}
	listed := map[string]any{"key": saved.Key, "client": fakeSurface, "enabled": true, "posters": []string{e2eHook}, "supervisor": map[string]any{"autonomy": "act", "turns_per_hour": 17}}
	cfg["scopes"] = append(cfg["scopes"].([]any), listed)
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, unlisted := range []bool{false, true} {
		if unlisted {
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
		}
		stop = runDaemon(t, r.root)
		r.connected(t)
		s = sessions.Load()
		policy := s.Scopes[saved.Key]
		person, ok := s.Core.People.Resolve(fakeSurface + ":" + e2eHook)
		if !ok || s.Core.People.May(person, saved.Key) == unlisted {
			t.Fatal("configured membership did not change with listing")
		}
		if policy.ConversationsOnly != unlisted {
			t.Fatalf("wrong policy: %+v", policy)
		}
		if !unlisted && (policy.Autonomy != supervisor.Act || s.Budgets[saved.Key].Limits.Turns != 17) {
			t.Fatalf("config did not win: %+v", policy)
		}
		current, err := s.Dir(ctx, saved.Key)
		if err != nil || current != dir {
			t.Fatalf("directory changed %q %v", current, err)
		}
		b, err := os.ReadFile(marker)
		if err != nil || string(b) != "retained notes" {
			t.Fatalf("notes changed: %q %v", b, err)
		}
		api, err = wire.Dial(r.root)
		if err != nil {
			t.Fatal(err)
		}
		got, err := api.Thread(ctx, id, 0, 0)
		if err != nil || got.ID != id || got.Kind != control.KindConversation {
			t.Fatalf("conversation changed: %+v %v", got, err)
		}
		scopes, err := api.Scopes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, sc := range scopes {
			if sc.Key == saved.Key {
				found = true
				if sc.Unlisted != unlisted {
					t.Fatalf("wire policy: %+v", sc)
				}
			}
		}
		if !found {
			t.Fatal("stored scope missing from wire")
		}
		api.Close()
		stop()
	}
}
