package store

import (
	"context"
	"testing"
	"time"
)

func TestAwayGraceCatchupAndPersistentActiveAge(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err := s.SyncMachines(ctx, []MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1000, 0)
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','one','owner','running',1000000,1000000,'build')`,
		`INSERT INTO sessions(id,thread_id,agent,state,spec,created_at,updated_at) VALUES(1,1,'fake','running','{}',1000000,1000000)`,
		`INSERT INTO machine_reservations(thread_id,machine,agent,operation,created_at) VALUES(1,'build','fake','original',1000000)`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.ObserveConnection(ctx, "build", true, "generation-1", "", false, 30*time.Second, t0)
	if err != nil || a.State != "catching_up" {
		t.Fatalf("%+v %v", a, err)
	}
	if err := s.FinishCatchup(ctx, "build", "generation-1", t0); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Availability(ctx, "build"); a.State == "ready" {
		t.Fatal("inventory alone bypassed session catch-up")
	}
	if err := s.AcknowledgeCatchup(ctx, 1, "generation-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCatchup(ctx, "build", "generation-1", t0); err != nil {
		t.Fatal(err)
	}
	a, err = s.ObserveConnection(ctx, "build", false, "generation-1", "transport", false, 30*time.Second, t0.Add(5*time.Second))
	if err != nil || a.State != "checking" || a.FirstFailure != t0.Add(5*time.Second).UnixMilli() {
		t.Fatalf("immediate dispatch block %+v %v", a, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.ObserveConnection(ctx, "build", false, "generation-1", "later reason", false, 30*time.Second, t0.Add(36*time.Second))
	if err != nil || a.State != "away" || a.Reason != "transport" || a.FirstFailure != t0.Add(5*time.Second).UnixMilli() {
		t.Fatalf("durable first failure %+v %v", a, err)
	}
	age, err := ActiveAge(ctx, s.DB, 1, t0, t0.Add(2*time.Hour))
	if err != nil || age != 5*time.Second {
		t.Fatalf("paused age %s %v", age, err)
	}
	a, err = s.ObserveConnection(ctx, "build", true, "generation-2", "", false, 30*time.Second, t0.Add(2*time.Hour))
	if err != nil || a.State != "catching_up" {
		t.Fatalf("reconnect %+v %v", a, err)
	}
	if err := s.AcknowledgeCatchup(ctx, 1, "generation-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCatchup(ctx, "build", "generation-2", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Availability(ctx, "build"); a.State == "ready" {
		t.Fatal("old generation acknowledged new connection")
	}
	if err := s.AcknowledgeCatchup(ctx, 1, "generation-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCatchup(ctx, "build", "generation-2", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	age, err = ActiveAge(ctx, s.DB, 1, t0, t0.Add(2*time.Hour+3*time.Second))
	if err != nil || age != 8*time.Second {
		t.Fatalf("remaining active time %s %v", age, err)
	}
	var slots int
	s.DB.QueryRow(`SELECT count(*) FROM machine_reservations`).Scan(&slots)
	if slots != 1 {
		t.Fatal("absence released original slot")
	}
	a, err = s.ObserveConnection(ctx, "build", false, "generation-2", "binding", true, 30*time.Second, t0.Add(2*time.Hour+4*time.Second))
	if err != nil || a.State != "away" {
		t.Fatalf("binding grace %+v %v", a, err)
	}
}
