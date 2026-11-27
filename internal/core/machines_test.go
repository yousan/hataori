package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

func TestMachineRestrictionsApplyBeforeOrdinaryStartEffects(t *testing.T) {
	for _, tc := range []struct {
		name             string
		machines, agents []string
		allowed          bool
	}{
		{"omitted", nil, nil, true},
		{"local", []string{"local"}, []string{"fake"}, true},
		{"empty machines", []string{}, nil, false},
		{"remote only", []string{"build"}, nil, false},
		{"empty agents", nil, []string{}, false},
		{"other agent", nil, []string{"other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fakeOwner{})
			sc := f.core.Scopes["s"]
			sc.Machines = tc.machines
			f.core.Scopes["s"] = sc
			f.core.LocalAgents = tc.agents
			id, err := f.owner.Start(context.Background(), control.StartRequest{Scope: "s", Thread: "t", Key: "start", Input: control.Input{Text: "go"}})
			if tc.allowed {
				if err != nil || id == 0 {
					t.Fatalf("start: %d %v", id, err)
				}
				var machine string
				if err := f.core.Store.DB.QueryRow(`SELECT machine FROM threads WHERE id=?`, int64(id)).Scan(&machine); err != nil || machine != "local" {
					t.Fatalf("local default: %s %v", machine, err)
				}
			} else {
				if !errors.Is(err, session.ErrRefused) || id != 0 {
					t.Fatalf("refusal: %d %v", id, err)
				}
				for _, table := range []string{"threads", "sessions", "inbox", "events"} {
					var n int
					if err := f.core.Store.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
						t.Fatalf("refused start changed %s: %d %v", table, n, err)
					}
				}
			}
		})
	}
}

func TestMachineConfigKeepsConfinedKindsLocalAndAuthorization(t *testing.T) {
	ctx := context.Background()
	k := newKinds(t)
	if err := k.core.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
		t.Fatal(err)
	}
	sc := k.core.Scopes["s"]
	sc.Machines = []string{}
	k.core.Scopes["s"] = sc
	k.core.LocalAgents = []string{}
	for _, kind := range []control.ThreadKind{control.KindSupervisor, control.KindConversation, control.KindLookup} {
		id := k.start(t, kind, string(kind))
		var machine, dir string
		if err := k.core.Store.DB.QueryRow(`SELECT machine,workspace FROM threads WHERE id=?`, int64(id)).Scan(&machine, &dir); err != nil || machine != "local" || dir != k.dir {
			t.Fatalf("confined thread moved: %s %s %v", machine, dir, err)
		}
	}
	outsider := k.core.As(control.Caller{Principal: "outsider", Scopes: []string{"s"}, May: all})
	if _, err := outsider.Start(ctx, control.StartRequest{Scope: "s", Thread: "unauthorized", Key: "unauthorized"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatalf("machines granted scope access: %v", err)
	}
}

func TestLimitsKeepMachineAndConfigDirectoryReportsSeparate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	if err := f.core.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
		t.Fatal(err)
	}
	_, a := f.running(t, "a")
	_, b := f.running(t, "b")
	for _, id := range []int64{a, b} {
		data, _ := json.Marshal(map[string]any{"spec": agent.Spec{ConfigDir: "same-spelling"}})
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET spec=? WHERE id=?`, string(data), id); err != nil {
			t.Fatal(err)
		}
	}
	// A remote report is recorded against a distinct machine, even with the
	// same directory spelling. No remote process or connection is started.
	if _, err := f.core.Store.DB.Exec(`INSERT INTO threads (id,scope,key,started_by,state,created_at,updated_at,machine) VALUES (90,'s','remote','owner','idle',1,1,'build')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.Store.DB.Exec(`INSERT INTO sessions (id,thread_id,agent,state,created_at,updated_at,spec) SELECT 90,90,'fake','idle',1,1,spec FROM sessions WHERE id=?`, a); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(2000)
	f.agentEvent(t, a, 2, limitsEvent(at, 20))
	f.agentEvent(t, 90, 1, limitsEvent(at.Add(time.Hour), 90))
	data, _ := json.Marshal(map[string]any{"spec": agent.Spec{ConfigDir: "other-directory"}})
	if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET spec=? WHERE id=?`, string(data), b); err != nil {
		t.Fatal(err)
	}
	f.agentEvent(t, b, 2, limitsEvent(at.Add(time.Minute), 30))
	var n int
	if err := f.core.Store.DB.QueryRow(`SELECT count(*) FROM agent_limits`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("reports conflated: %d %v", n, err)
	}
	ls, err := f.owner.Limits(ctx)
	if err != nil || len(ls) != 1 || ls[0].Windows[0].Used != 30 {
		t.Fatalf("legacy view mixed/summed remote allowance: %+v %v", ls, err)
	}
	// An older update to one account never erases its newer snapshot.
	f.agentEvent(t, a, 3, limitsEvent(at.Add(-time.Minute), 1))
	var windows string
	if err := f.core.Store.DB.QueryRow(`SELECT windows FROM agent_limits WHERE machine='local' AND config_dir='same-spelling'`).Scan(&windows); err != nil {
		t.Fatal(err)
	}
	var ws []control.LimitWindow
	if err := json.Unmarshal([]byte(windows), &ws); err != nil || len(ws) != 1 || ws[0].Used != 20 {
		t.Fatalf("old report overwrote account: %s %v", windows, err)
	}
}
