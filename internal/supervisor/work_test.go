package supervisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
)

func workCore(f *filesFixture) *core.Core {
	return &core.Core{Store: f.files.Store, Log: f.files.Log, People: people.New(nil, "p", nil),
		Scopes: map[string]core.Scope{"c1": {Enabled: true}, "c2": {Enabled: true}}}
}

func workMCP(f *filesFixture, scope string, thread control.ThreadID) *MCP {
	api := workCore(f).As(control.Caller{Principal: "p", Scopes: []string{scope}, May: control.OpRead})
	return &MCP{Scope: scope, Dial: func() (control.API, error) { return f.files.API(api, scope, thread), nil }}
}

func writeWork(t *testing.T, m *MCP, r WorkRecord, version string) (string, bool) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"op": "write", "record": r, "version": version})
	if err != nil {
		t.Fatal(err)
	}
	return call(t, m, ToolWork, string(b))
}

// Real scope-file CAS, durable reminders and wake preparation, without a
// process/provider/terminal. The two conditions remain data on the same path.
func TestWorkRecordNormalAndRestart(t *testing.T) {
	for _, condition := range []string{"Reviewed merge and exact-main checks", "Two primary sources and explicit uncertainty"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			f := newFilesFixture(t)
			m := workMCP(f, "c1", filesOwn)
			r := WorkRecord{Schema: 1, Objective: "Answer the accepted request", Completion: condition,
				SourceThread: filesConv, ReportThread: filesOwn, State: "waiting", Wait: "external evidence",
				Next: "Read evidence on the accepted reminder; report in own supervisor thread"}
			if got, bad := writeWork(t, m, r, ""); bad || !strings.Contains(got, "stored") {
				t.Fatalf("create: %s", got)
			}
			before := holds(t, f.dir, WorkPath)
			if got, bad := writeWork(t, m, r, "stale"); !bad || !strings.Contains(got, "unchanged") || holds(t, f.dir, WorkPath) != before {
				t.Fatalf("stale update: %s", got)
			}
			c := workCore(f)
			now := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
			reminders := NewReminders(c, "c1", 10, c.Log)
			reminders.Clock = at(now)
			if _, err := reminders.Remind(ctx, control.RemindRequest{Delay: 600, Note: "Read notes/work.json"}); err != nil {
				t.Fatal(err)
			}
			// Close and reopen the actual store and all scope wrappers. No
			// task input is reposted and no reminder is recreated.
			if err := f.files.Store.Close(); err != nil {
				t.Fatal(err)
			}
			f = openFilesFixture(t, f.root)
			m = workMCP(f, "c1", filesOwn)
			if got, bad := call(t, m, ToolWork, `{"op":"read"}`); bad || !strings.Contains(got, condition) || !strings.Contains(got, "external evidence") {
				t.Fatalf("read after reopen: %s", got)
			}
			c = workCore(f)
			reminders = NewReminders(c, "c1", 10, c.Log)
			reminders.Clock = at(now.Add(11 * time.Minute))
			w := &Waker{Core: c, Scope: "c1", Owner: "p", Reminders: reminders, Clock: reminders.Clock, Log: c.Log, ThreadsMax: 20}
			wake := newWaking(w)
			wake.sv = filesOwn
			wake.api = c.As(control.Caller{Principal: "p", Scopes: []string{"c1"}, May: control.OpRead})
			prepared, ok, err := wake.prepare(ctx)
			if err != nil || !ok || !strings.Contains(prepared.text, condition) || !strings.Contains(prepared.text, `"state":"waiting"`) ||
				!strings.Contains(prepared.text, "ordinary final answer here") {
				t.Fatalf("permitted reminder recovery: %+v, %v, %v", prepared, ok, err)
			}
			// A model assessment is explicitly distinct from a report receipt.
			r.State, r.Wait, r.Next, r.Evidence = "complete", "", "Report the answer", "model assessment: sources checked"
			if got, bad := writeWork(t, m, r, versionOf(before)); bad || !strings.Contains(got, "No wake, action or report was sent") {
				t.Fatalf("update: %s", got)
			}
			if got := w.workContext(ctx, filesOwn); !strings.Contains(got, `"state":"complete"`) || !strings.Contains(got, "not report delivery") || !strings.Contains(got, "do not start another action or repeat its report") {
				t.Fatalf("assessment context: %s", got)
			}
		})
	}
}

func TestWorkRecordScopeAndInvalidData(t *testing.T) {
	f := newFilesFixture(t)
	m := workMCP(f, "c1", filesOwn)
	r := WorkRecord{Schema: 1, Objective: "private request", Completion: "user's condition", SourceThread: filesOwn,
		ReportThread: filesOwn, State: "active", Next: "Wait for evidence"}
	if got, bad := writeWork(t, m, r, ""); bad {
		t.Fatal(got)
	}
	before := holds(t, f.dir, WorkPath)
	for _, change := range []func(*WorkRecord){
		func(r *WorkRecord) { r.SourceThread = filesOther },
		func(r *WorkRecord) { r.ReportThread = filesOther },
		func(r *WorkRecord) { r.ReportThread = filesConv },
		func(r *WorkRecord) { r.Worker = filesOther },
		func(r *WorkRecord) { r.State = "delivered" },
		func(r *WorkRecord) { r.Schema = 2 },
	} {
		badRecord := r
		change(&badRecord)
		if got, bad := writeWork(t, m, badRecord, versionOf(before)); !bad || holds(t, f.dir, WorkPath) != before {
			t.Fatalf("invalid update changed work: %s", got)
		}
	}
	other := workMCP(f, "c2", filesOther)
	if got, bad := call(t, other, ToolWork, `{"op":"read"}`); !bad || strings.Contains(got, "private request") {
		t.Fatalf("other scope read private work: %s", got)
	}
	// Even a forged CLI scope cannot move the token-bound Files directory.
	m.Scope = "c2"
	if got, bad := writeWork(t, m, r, versionOf(before)); !bad || holds(t, f.dir, WorkPath) != before {
		t.Fatalf("forged scope: %s", got)
	}
	m.Scope = "c1"
	for _, invalid := range []string{`{"schema":99}`, `not JSON`, strings.Repeat("x", workMax+1),
		strings.Replace(before, `"schema": 1`, `"schema": 1, "verified": true`, 1)} {
		put(t, f.dir, WorkPath, invalid)
		if got, bad := call(t, m, ToolWork, `{"op":"read"}`); !bad {
			t.Fatalf("invalid record read: %s", got)
		}
		w := &Waker{Core: workCore(f), Scope: "c1", Owner: "p", Log: f.files.Log}
		if got := w.workContext(context.Background(), filesOwn); !strings.Contains(got, "blocked") || holds(t, f.dir, WorkPath) != invalid {
			t.Fatalf("invalid context was not held intact: %s", got)
		}
	}
}
