package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/supervisor"
)

func TestSupervisorAgentSelectionOwnConversationAndWake(t *testing.T) {
	sessions := supervisors(t)
	wt := newWakeTest(t, func(cfg map[string]any) {
		cfg["agent"] = fake.Name
		cfg["model"] = "worker-model"
		cfg["adapters"].(map[string]any)[secondAgent] = map[string]any{"command": []string{os.Args[0], "fake"}, "env": []string{"PATH=" + os.Getenv("PATH")}, "confines": true, "model": "selected-model"}
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["agent"] = secondAgent
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	if sv.Agent != secondAgent || sv.Model != "selected-model" {
		t.Fatalf("supervisor got worker defaults: %+v", sv)
	}
	id, err := sessions.Load().Converse(context.Background(), supervisor.Call{Scope: "c1", Key: "selected-talk", From: people.Join(fakeSurface, e2eOwner), Text: "whoami"})
	if err != nil {
		t.Fatal(err)
	}
	wt.ended(t, id, 1)
	th, err := owner.Thread(context.Background(), id, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if th.Agent != secondAgent || th.Model != "selected-model" {
		t.Fatalf("conversation used wrong adapter defaults: %+v", th)
	}
	worker, pos := wt.worker(t, owner, "c1", "ordinary", "reply completed")
	th, err = owner.Thread(context.Background(), worker, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if th.Agent != fake.Name || th.Model != "worker-model" {
		t.Fatalf("supervisor selection changed ordinary worker: %+v", th)
	}
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1)
	var agentName, model string
	if err := wt.db.QueryRow(`SELECT agent,model FROM sessions WHERE thread_id=? ORDER BY id DESC LIMIT 1`, int64(sv.ID)).Scan(&agentName, &model); err != nil {
		t.Fatal(err)
	}
	if agentName != secondAgent || model != "selected-model" {
		t.Fatalf("wake: %s %s", agentName, model)
	}
}

// Switching closes the old coordinator, never its workers or directory. The
// new session receives no old-provider resume id; old records stay readable.
func TestSupervisorAgentSwitchPreservesScopeAndWorkers(t *testing.T) {
	wt := newWakeTest(t, func(cfg map[string]any) {
		cfg["agent"] = fake.Name
		cfg["adapters"].(map[string]any)[secondAgent] = map[string]any{"command": []string{os.Args[0], "fake"}, "env": []string{"PATH=" + os.Getenv("PATH")}, "confines": true}
	})
	stop, owner, sv := wt.start(t)
	defer func() { owner.Close(); stop() }()
	ctx := context.Background()
	if a := sendAndRead(t, owner, sv.ID, "note", `mcp write_file {"path":"notes/switch.md","text":"keep-this-history"}`); a == "" {
		t.Fatal("no note reply")
	}
	if a := sendAndRead(t, owner, sv.ID, "remind", `mcp set_reminder {"minutes":120,"note":"keep-reminder"}`); !strings.Contains(a, "Reminder") {
		t.Fatal(a)
	}
	worker, _ := wt.worker(t, owner, "c1", "keep-worker", "reply completed")
	var dir string
	wt.db.QueryRow(`SELECT dir FROM supervisor_dirs WHERE scope='c1'`).Scan(&dir)
	if err := owner.CloseThread(ctx, sv.ID); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	stop()
	svConfig(t, wt.r.root, map[string]any{"agent": secondAgent})
	var next control.ThreadInfo
	stop, owner, next = wt.start(t)
	if next.ID == sv.ID || next.Agent != secondAgent {
		t.Fatalf("not a fresh selected supervisor: %+v", next)
	}
	b, err := os.ReadFile(dir + "/notes/switch.md")
	if err != nil || string(b) != "keep-this-history" {
		t.Fatalf("scope notes lost: %q, %v", b, err)
	}
	old, err := owner.Thread(ctx, sv.ID, 100, 0)
	if err != nil || old.State != control.Closed {
		t.Fatal("old transcript was not retained", err)
	}
	th, err := owner.Thread(ctx, worker, 100, 0)
	if err != nil || th.State == control.Closed || th.Agent != fake.Name {
		t.Fatal("worker changed during switch", err)
	}
	var reminders int
	wt.db.QueryRow(`SELECT count(*) FROM supervisor_reminders WHERE note='keep-reminder' AND state='open'`).Scan(&reminders)
	if reminders != 1 {
		t.Fatal("switch lost outstanding reminder")
	}
	var n int
	wt.db.QueryRow(`SELECT count(*) FROM threads WHERE confined=0`).Scan(&n)
	if n != 1 {
		t.Fatalf("switch duplicated worker: %d", n)
	}
	// Scope reminders and worker ownership have scope identities, not adapters.
	var newDir string
	wt.db.QueryRow(`SELECT dir FROM supervisor_dirs WHERE scope='c1'`).Scan(&newDir)
	if newDir != dir {
		t.Fatal("switch replaced scope directory")
	}
	// The same operator procedure supports rollback without resurrecting the
	// closed session or replaying another provider's transcript.
	if err := owner.CloseThread(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	stop()
	svConfig(t, wt.r.root, map[string]any{"agent": fake.Name})
	var back control.ThreadInfo
	stop, owner, back = wt.start(t)
	if back.ID == sv.ID || back.ID == next.ID || back.Agent != fake.Name {
		t.Fatalf("rollback resumed the wrong native conversation: %+v", back)
	}
	b, _ = os.ReadFile(dir + "/notes/switch.md")
	if string(b) != "keep-this-history" {
		t.Fatal("rollback lost notes")
	}
}
