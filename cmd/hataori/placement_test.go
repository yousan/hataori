package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

func placementConfigStore(t *testing.T) (string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, st
}
func TestPlacementModeSurvivesRemovingLastLimitAndRevocation(t *testing.T) {
	for _, phase := range []int{0, 1, 2} {
		t.Run(string(rune('0'+phase)), func(t *testing.T) {
			root, st := placementConfigStore(t)
			t.Cleanup(func() { st.Close() })
			ctx := context.Background()
			cfg := &instance.Config{Machines: []instance.Machine{{Key: "local", MaxResident: 1}}, People: people.New(nil, "owner", nil)}
			c := &core.Core{Store: st}
			r := &runner.Runner{Store: st}
			if err := configurePlacement(ctx, root, cfg, c, r, nil, nil); err != nil || c.Placement == nil {
				t.Fatal("placement was not enabled", err)
			}
			raw, _ := json.Marshal(core.FirstInput{AcceptedOperation: control.OpStart, Principal: "owner", From: "plugin:retired", Text: "do not deliver"})
			if _, err := st.DB.Exec(`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','legacy','owner','creating',1,1,'local');
   INSERT INTO sessions(id,thread_id,agent,state,spec,first_input,launch,created_at,updated_at) VALUES(1,1,'fake','creating','{}',?,?,1,1);
   INSERT INTO machine_reservations(thread_id,machine,agent,operation,created_at) VALUES(1,'local','fake','legacy-placement-1',1)`, string(raw), phase); err != nil {
				t.Fatal(err)
			}
			st.Close()
			var err error
			st, err = store.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Machines = []instance.Machine{{Key: "local"}}
			c = &core.Core{Store: st, Machine: &session.Machine{Store: st}, People: cfg.People, Scopes: map[string]core.Scope{"s": {Enabled: true}}, Agent: "fake", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			r = &runner.Runner{Store: st, Root: root, Log: c.Log, Machine: c.Machine}
			c.Runner = r
			if err := configurePlacement(ctx, root, cfg, c, r, nil, nil); err != nil {
				t.Fatal(err)
			}
			if c.Placement == nil || r.RevalidateLaunch == nil {
				t.Fatal("removing last limit disabled durable authorization")
			}
			if err := r.Reconnect(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			var got int
			var retained string
			if err := st.DB.QueryRow(`SELECT launch,first_input FROM sessions WHERE id=1`).Scan(&got, &retained); err != nil || got != core.PlacementLaunchCanceled || retained != string(raw) {
				t.Fatal("revoked launch revived after configuration change", got, retained, err)
			}
			rs, err := st.MachineReservations(ctx)
			if err != nil || len(rs) != 1 {
				t.Fatal("cancellation lost host reservation", rs, err)
			}
		})
	}
}
func TestPlacementNeverEnabledLocalAndModeReadFailure(t *testing.T) {
	root, st := placementConfigStore(t)
	defer st.Close()
	ctx := context.Background()
	cfg := &instance.Config{Machines: []instance.Machine{{Key: "local"}}, People: people.New(nil, "owner", nil)}
	c := &core.Core{Store: st}
	r := &runner.Runner{Store: st}
	if err := configurePlacement(ctx, root, cfg, c, r, nil, nil); err != nil || c.Placement != nil || r.Admit != nil || r.RevalidateLaunch != nil {
		t.Fatal("never-enabled default changed", err)
	}
	st.Close()
	if err := configurePlacement(ctx, root, cfg, c, r, nil, nil); err == nil {
		t.Fatal("mode read failure silently disabled protection")
	}
}
