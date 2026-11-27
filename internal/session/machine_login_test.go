package session

import (
	"context"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

func TestLoginOutagesOfDifferentMachinesDoNotJoinOrClose(t *testing.T) {
	ctx := context.Background()
	m, a := newMachine(t, control.Idle)
	if err := m.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.DB.Exec(`INSERT INTO threads (id,scope,key,started_by,state,created_at,updated_at,machine) VALUES (2,'s','remote','p','idle',1,1,'build')`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.DB.Exec(`INSERT INTO sessions (id,thread_id,agent,state,created_at,updated_at) VALUES (2,2,'fake','idle',1,1)`); err != nil {
		t.Fatal(err)
	}
	setConfigDir(t, m, a, "same-spelling")
	setConfigDir(t, m, 2, "same-spelling")
	turn(t, m, a, "local-login", agent.Failed, agent.Login)
	turn(t, m, 2, "remote-login", agent.Failed, agent.Login)
	var n int
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM login_outages WHERE closed_at IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("machines joined outages: %d %v", n, err)
	}
	turn(t, m, a, "local-fixed", agent.Completed, "")
	var remote int64
	if err := m.Store.DB.QueryRow(`SELECT id FROM login_outages WHERE machine='build' AND closed_at IS NULL`).Scan(&remote); err != nil {
		t.Fatal("local completion closed remote outage:", err)
	}
	if _, ok, err := OutageLogin(ctx, m.Store.DB, remote); err != nil || ok {
		t.Fatalf("main relay accepted a remote outage: %v %v", ok, err)
	}
	if err := m.Store.Update(ctx, func(tx *store.Tx) error {
		closed, err := CloseLoginOutage(ctx, tx, Login{Agent: "fake", Dir: "same-spelling"})
		if closed {
			t.Fatal("main relay closed remote outage")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A worker directory is not resolved through main's environment/defaults.
	m.LoginDir = func(string, agent.Spec) string { t.Fatal("remote login used main's configuration resolver"); return "" }
	turn(t, m, 2, "remote-fixed", agent.Completed, "")
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM login_outages WHERE closed_at IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("remote completion: %d %v", n, err)
	}
}
