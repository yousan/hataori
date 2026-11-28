package session

import (
	"context"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

func TestWorkerLoginClosesOnlyOriginalMachineOutage(t *testing.T) {
	ctx := context.Background()
	m, _ := newMachine(t, control.Idle)
	if err := m.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "worker", Root: "/fixture/worker"}, {Key: "other", SSH: "other", Root: "/fixture/other"}}); err != nil {
		t.Fatal(err)
	}
	for _, machine := range []string{"local", "build", "other"} {
		if _, err := m.Store.DB.Exec(`INSERT INTO login_outages(machine,agent,config_dir,opened_at) VALUES (?,'fake','same-directory',1)`, machine); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Store.DB.Exec(`INSERT INTO login_outage_threads(outage_id,thread_id) SELECT id,1 FROM login_outages`); err != nil {
		t.Fatal(err)
	}
	var original int64
	if err := m.Store.DB.QueryRow(`SELECT id FROM login_outages WHERE machine='build'`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	l, ok, err := WorkerOutage(ctx, m.Store.DB, original)
	if err != nil || !ok || !l.Open {
		t.Fatalf("outage lookup: %v %v", ok, err)
	}
	closeExpected := func(expected WorkerLogin, want bool) {
		t.Helper()
		if err := m.Store.Update(ctx, func(tx *store.Tx) error {
			closed, err := CloseWorkerLoginOutage(ctx, tx, original, expected)
			if closed != want {
				t.Errorf("closed = %v, want %v", closed, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	wrong := l
	wrong.Machine = "other"
	closeExpected(wrong, false)
	wrong = l
	wrong.Agent = "another"
	closeExpected(wrong, false)
	wrong = l
	wrong.Dir = "another-directory"
	closeExpected(wrong, false)
	closeExpected(l, true)
	if _, err := m.Store.DB.Exec(`INSERT INTO login_outages(machine,agent,config_dir,opened_at) VALUES ('build','fake','same-directory',2)`); err != nil {
		t.Fatal(err)
	}
	closeExpected(l, false)
	var n int
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM login_outages WHERE closed_at IS NULL`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("another machine or later outage closed: %d %v", n, err)
	}
}
