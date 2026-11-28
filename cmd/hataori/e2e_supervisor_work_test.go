package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// One initial task, existing reminder, fresh wake and ordinary report. The
// fake model's assessment is scripted; this proves mechanics, not judgement.
// No real CLI/provider/terminal is involved.
func TestSupervisorWorkRecordLoop(t *testing.T) {
	for _, tc := range []struct {
		name, condition string
		restart         bool
	}{
		{"normal", "Reviewed merge and exact-main checks", false},
		{"restart", "Two attributed primary sources and uncertainty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := newWakeTest(t, budget(0, 0, 0), func(cfg map[string]any) {
				cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["instructions"] = "wake-script"
			})
			stop, owner, sv := wt.start(t)
			defer func() { stop() }()
			ctx := context.Background()
			conn := func() *wire.Client {
				t.Helper()
				c, err := wire.DialToken(wt.r.root, tokenOf(t, wt.r.root, sv.ID))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				return c
			}
			api := conn()
			r := supervisor.WorkRecord{Schema: 1, Objective: "Answer the accepted request", Completion: tc.condition,
				SourceThread: sv.ID, ReportThread: sv.ID, State: "waiting", Wait: "evidence; reminder unconfirmed",
				Next: "At the next permitted reminder, read the work and report"}
			b, _ := json.Marshal(map[string]any{"op": "write", "record": r})
			if got := sendAndRead(t, owner, sv.ID, "initial-work", "mcp work_record "+string(b)); !strings.HasPrefix(got, "mcp: Work record stored") {
				t.Fatalf("initial record: %s", got)
			}
			stored, err := api.Files(ctx, control.FilesRequest{Op: control.FilesRead, Path: supervisor.WorkPath})
			if err != nil || stored.File == nil {
				t.Fatalf("read: %+v %v", stored, err)
			}
			r.State, r.Wait, r.Next, r.Evidence = "complete", "", "Report to the recorded supervisor thread", "fake model: condition assessed"
			update, _ := json.Marshal(map[string]any{"op": "write", "version": stored.File.Version, "record": r})
			report := "Accepted request answered under policy: " + tc.condition
			// Scripted model reads the actual durable file, records its
			// assessment with CAS, then renders a normal final report.
			note := "mcp work_record {\"op\":\"read\"}\nmcp work_record " + string(update) + "\nreply " + report
			remindIn(t, api, 10*time.Minute, note)
			if tc.restart {
				stop()
				wt.clock().advance(11 * time.Minute)
				stop, owner, _ = wt.start(t)
				api = conn()
			} else {
				wt.steps(11 * time.Minute)
			}
			wake := wt.wokenN(t, owner, sv.ID, 1)[0]
			if !strings.Contains(wake, "Durable work record") || !strings.Contains(wake, tc.condition) ||
				!strings.Contains(wake, `"state":"waiting"`) || !strings.Contains(wake, fmt.Sprintf(`"report_thread":%d`, sv.ID)) {
				t.Fatalf("wake did not recover record: %s", wake)
			}
			conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
			wt.r.posted(t, conv, report)
			stored, err = api.Files(ctx, control.FilesRequest{Op: control.FilesRead, Path: supervisor.WorkPath})
			var completed supervisor.WorkRecord
			if err != nil || stored.File == nil || json.Unmarshal([]byte(stored.File.Text), &completed) != nil || completed.State != "complete" {
				t.Fatalf("model CAS assessment: %+v, %v", stored, err)
			}
			wt.noWake(t, sv.ID, 20*time.Minute, 1, "after the one-shot reminder")
			count := 0
			for _, post := range postsIn(wt.r.driver, conv) {
				if post.Message.Text == report {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("ordinary reports = %d, want 1", count)
			}
		})
	}
}
