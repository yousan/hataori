package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/placement"
)

func seedPlacementLocal(t *testing.T, f *fixture) {
	t.Helper()
	_, err := f.st.DB.Exec(`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','placement-local','owner','creating',1,1,'local');
 INSERT INTO sessions(id,thread_id,agent,state,spec,created_at,updated_at,launch) VALUES(1,1,'fake','failed','{}',1,1,0),(2,1,'fake','creating','{}',1,1,2)`)
	if err != nil {
		t.Fatal(err)
	}
}
func placementEndFile(t *testing.T, f *fixture, id int64) {
	t.Helper()
	dir := StateDir(f.root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(host.Status{Exited: true})
	if err := os.WriteFile(filepath.Join(dir, host.StatusFile), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestPlacementLocalPriorUnknownBlocksReplacementAndRelease(t *testing.T) {
	f := newFixture(t, options{})
	seedPlacementLocal(t, f)
	f.run.endWait = time.Millisecond
	ctx := context.Background()
	if err := f.run.endPriorPlacements(ctx, 2); !errors.Is(err, core.ErrPlacementWait) {
		t.Fatal("unknown prior host admitted replacement", err)
	}
	placementEndFile(t, f, 2)
	if ended, err := f.run.PlacementEnded(ctx, 2); err != nil || ended {
		t.Fatal("latest end released unknown earlier host", ended, err)
	}
	placementEndFile(t, f, 1)
	if err := f.run.endPriorPlacements(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if ended, err := f.run.PlacementEnded(ctx, 2); err != nil || !ended {
		t.Fatal(ended, err)
	}
}
func TestPlacementLocalRecoveryUsesOriginalHostBeforeNewAdmission(t *testing.T) {
	f := newFixture(t, options{})
	seedPlacementLocal(t, f)
	ctx := context.Background()
	f.core.Placement = &core.MachinePlacement{Machines: map[string]core.PlacementMachine{"local": {Limits: placement.Limits{MaxResident: 1}}}, Observe: func(context.Context, string, string) (placement.Observation, error) {
		return placement.Observation{}, nil
	}}
	raw, _ := json.Marshal(core.FirstInput{Principal: "owner"})
	if _, err := f.st.DB.Exec(`UPDATE sessions SET first_input=? WHERE id=2; INSERT INTO machine_reservations(thread_id,machine,agent,operation,created_at) VALUES(1,'local','fake','placement-original',1)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := f.core.AdmitSession(ctx, 2); !errors.Is(err, core.ErrPlacementWait) {
		t.Fatal("unstarted phase2 bypassed unknown resource gate", err)
	}
	placementEndFile(t, f, 2)
	if err := os.WriteFile(filepath.Join(StateDir(f.root, 2), host.HostPIDFile), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.core.AdmitSession(ctx, 2); err != nil {
		t.Fatal("original completed host blocked by current resource use", err)
	}
	// This is the previous wake's PID, not proof of the phase2 start.
	if _, err := f.st.DB.Exec(`UPDATE sessions SET host_pid=12345 WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := f.core.AdmitSession(ctx, 2); !errors.Is(err, core.ErrPlacementWait) {
		t.Fatal("prior run PID admitted a new start", err)
	}
}

func TestPlacementRevocationStaysCanceledAcrossTwoRestarts(t *testing.T) {
	f := newFixture(t, options{})
	ctx := context.Background()
	f.core.Placement = &core.MachinePlacement{Machines: map[string]core.PlacementMachine{"local": {Limits: placement.Limits{MaxResident: 1}}}, Observe: func(context.Context, string, string) (placement.Observation, error) {
		now := time.Now()
		return placement.Observation{Ready: true, Reconciled: true, Known: true, Installed: true, LoggedIn: true, At: now, HealthAt: now, ValidFor: 10 * time.Second, Memory: 1 << 30, Disk: 1 << 30}, nil
	}}
	f.run.Admit = f.core.AdmitSession
	lost := false
	f.run.point = func(point string) {
		if point == "after host start" {
			panic("isolated lost start response")
		}
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != "isolated lost start response" {
				t.Fatalf("unexpected start recovery: %v", recovered)
			}
			lost = true
		}()
		f.start("reply revoked-input")
	}()
	if !lost {
		t.Fatal("did not reach successful host start")
	}
	var phase int
	if err := f.st.DB.QueryRow(`SELECT launch FROM sessions WHERE id=1`).Scan(&phase); err != nil || phase != 2 {
		t.Fatal(phase, err)
	}
	var first string
	f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=1`).Scan(&first)
	f.core.Placement.Caller = func(context.Context, control.Caller, string, string) (control.Caller, error) {
		return control.Caller{}, control.ErrForbidden
	}
	restart := func() {
		f.run.Close()
		next := &Runner{Store: f.st, Machine: f.core.Machine, Adapters: f.run.Adapters, Root: f.root, Host: f.run.Host, Log: f.run.Log, Poll: time.Millisecond, Admit: f.core.AdmitSession}
		f.run = next
		f.core.Runner = next
	}
	restart()
	if err := f.run.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB.QueryRow(`SELECT launch FROM sessions WHERE id=1`).Scan(&phase); err != nil || phase != core.PlacementLaunchCanceled {
		t.Fatal("revocation did not retain canceled dispatch", phase, err)
	}
	restart()
	if err := f.run.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.core.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	f.run.mu.Lock()
	followers := len(f.run.live)
	f.run.mu.Unlock()
	if followers != 0 {
		t.Fatal("second restart followed canceled first input", followers)
	}
	got := counts(t, f.st)
	if got["input"] != 0 || got["turn_started"] != 0 {
		t.Fatal("revoked first input was delivered", got)
	}
	var retained string
	f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=1`).Scan(&retained)
	reservations, err := f.st.MachineReservations(ctx)
	if err != nil || len(reservations) != 1 || retained != first {
		t.Fatal("revocation lost host ownership or input audit", reservations, err)
	}
}
