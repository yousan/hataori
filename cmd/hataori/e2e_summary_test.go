package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/wire"
)

// `hataori threads` shows the agent of a thread, the model and effort the
// fake agent reports instead of the ones asked for, and its turns, which go
// up by one after a turn.
func TestThreadsShowAgentModelEffortAndTurns(t *testing.T) {
	root := newRoot(t)
	cfg := map[string]any{
		"owner": "owner", "trigger": "!go", "idle": "1h",
		"scopes": []map[string]any{{"key": "s", "client": "chat", "enabled": true}},
		"adapters": map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")},
			ReportModel: "reported-model", ReportEffort: "reported-effort"}},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, instance.ConfigFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	startDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply one"}, Model: "asked-model", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	// threads returns the one thread once it is idle after turn n.
	threads := func(n float64) map[string]any {
		t.Helper()
		var ts []map[string]any
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			code, out, _ := cli(t, "threads", "--root", root)
			ts = lines(t, out)
			if code == 0 && len(ts) == 1 && ts[0]["state"] == "idle" && ts[0]["turns"] == n {
				return ts[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("threads never showed turn %v idle: %v", n, ts)
			}
		}
	}
	got := threads(1)
	if got["agent"] != fake.Name || got["model"] != "reported-model" || got["effort"] != "reported-effort" {
		t.Fatalf("threads: %v", got)
	}
	if err := c.Send(ctx, id, "k2", control.Input{Text: "reply two"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	threads(2)
}
