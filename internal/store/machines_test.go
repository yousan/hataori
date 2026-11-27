package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type oldTable struct{ columns, rows []string }

func oldTables(t *testing.T, db *sql.DB) map[string]oldTable {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name != 'schema_version' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	out := map[string]oldTable{}
	for _, name := range names {
		r, err := db.Query(`SELECT * FROM "` + name + `" LIMIT 0`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := r.Columns()
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[name] = oldTable{cols, readOldColumns(t, db, name, cols)}
	}
	return out
}

func readOldColumns(t *testing.T, db *sql.DB, name string, cols []string) []string {
	t.Helper()
	quoted := make([]string, len(cols))
	for i, col := range cols {
		quoted[i] = `"` + col + `"`
	}
	rows, err := db.Query(`SELECT ` + strings.Join(quoted, ",") + ` FROM "` + name + `"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(data))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestMachineMigrationFromPreviousVersionCopy(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	compressed, err := os.ReadFile("testdata/machine-local/previous.db.gz")
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	z.Close()
	dbPath := filepath.Join(root, File)
	if err := os.WriteFile(dbPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := version(ctx, db)
	if err != nil || v != 53 {
		t.Fatalf("previous version: %d %v", v, err)
	}
	before := oldTables(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string]string{
		"workspaces/legacy/room/thread/untracked.txt":            "the only copy of this work\n",
		"workspaces/legacy/room/thread/.git/refs/heads/unpushed": "synthetic-unpushed-object\n",
		"workspaces/legacy/room/thread/.git/refs/stash":          "synthetic-stash-object\n",
		"workspaces/legacy/room/thread/nested/.git/HEAD":         "ref: refs/heads/private\n",
		"workspaces/legacy/room/thread/submodule/.git":           "gitdir: ../.git/modules/submodule\n",
		"state/sessions/41/events.ndjson":                        "opaque prior event bytes\n",
		"state/sessions/41/terminal.log":                         "prior terminal bytes\n",
		"confined/legacy/AGENTS.md":                              "shared scope notes\n",
		"confined/legacy/synthetic-token":                        "synthetic local-only token\n",
	} {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	type diskEntry struct {
		info fs.FileInfo
		data string
	}
	disk := map[string]diskEntry{}
	for _, dir := range []string{"workspaces", "confined", "state/sessions"} {
		if err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			var data []byte
			if !entry.IsDir() {
				data, err = os.ReadFile(path)
				if err != nil {
					return err
				}
			}
			disk[path] = diskEntry{info, string(data)}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"migrated", "reopened"} {
		for name, want := range before {
			if got := readOldColumns(t, s.DB, name, want.columns); !reflect.DeepEqual(got, want.rows) {
				t.Errorf("%s: old table %s changed: %v != %v", pass, name, got, want.rows)
			}
		}
		var local, total int
		if err := s.DB.QueryRow(`SELECT count(*), count(CASE WHEN machine = 'local' THEN 1 END) FROM threads`).Scan(&total, &local); err != nil || local != 4 || total != local {
			t.Fatalf("%s: local migration %d/%d: %v", pass, local, total, err)
		}
		for path, want := range disk {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(info, want.info) || info.Mode() != want.info.Mode() {
				t.Errorf("%s: file/directory replaced: %s", pass, path)
			}
			if !info.IsDir() {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want.data {
					t.Errorf("%s: file changed: %s", pass, path)
				}
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if pass == "migrated" {
			s, err = Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, err := os.ReadFile("testdata/machine-local/previous.db.gz"); err != nil || !bytes.Equal(got, compressed) {
		t.Fatal("source fixture changed")
	}
}

func machineStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := newRoot(t)
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}

func machineThread(t *testing.T, s *Store, id int64, machine any, kind string) {
	t.Helper()
	_, err := s.DB.Exec(`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at, machine, kind, confined) VALUES (?, 's', ?, 'ada', 'creating', 1, 1, ?, ?, ?)`, id, fmt.Sprint(id), machine, kind, kind != "ordinary")
	if err != nil {
		t.Fatal(err)
	}
}

func TestMachineBindingsSurviveRemovalAndRefuseRebinding(t *testing.T) {
	ctx := context.Background()
	s, root := machineStore(t)
	original := MachineBinding{Key: "build", SSH: "worker-alias", Root: "/srv/worker"}
	if err := s.SyncMachines(ctx, []MachineBinding{original}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindMachineInstallation(ctx, "build", "installation-a"); err != nil {
		t.Fatal(err)
	}
	machineThread(t, s, 1, "build", "ordinary")
	if err := s.SyncMachines(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ms, err := s.Machines(ctx)
	if err != nil || len(ms) != 2 || ms[0].Key != "build" || ms[0].Configured || ms[0].Installation != "installation-a" || !ms[1].Configured {
		t.Fatalf("removed identity: %+v %v", ms, err)
	}
	if err := s.BindMachineInstallation(ctx, "build", "installation-b"); err == nil {
		t.Fatal("removed machine rebound")
	}
	for _, changed := range []MachineBinding{
		{Key: "build", SSH: "other", Root: original.Root}, {Key: "build", SSH: original.SSH, Root: "/srv/other"},
	} {
		// Even a partial sync is rolled back if another key is invalid.
		err := s.SyncMachines(ctx, []MachineBinding{{Key: "new", SSH: "new", Root: "/srv/new"}, changed})
		if !errors.Is(err, ErrMachineBinding) {
			t.Fatalf("rebind: %v", err)
		}
		got, err := s.Machines(ctx)
		if err != nil || !reflect.DeepEqual(got, ms) {
			t.Fatalf("failed sync changed identity: %+v %v", got, err)
		}
	}
	if err := s.SyncMachines(ctx, []MachineBinding{original}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindMachineInstallation(ctx, "build", "installation-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindMachineInstallation(ctx, "build", "installation-b"); !errors.Is(err, ErrMachineBinding) {
		t.Fatalf("installation changed: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE threads SET machine='local' WHERE id=1`); err == nil {
		t.Fatal("placed thread moved")
	}
}

func TestMachinePlacementRecordsReopenAndReconcileSameThread(t *testing.T) {
	ctx := context.Background()
	s, root := machineStore(t)
	if err := s.SyncMachines(ctx, []MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
		t.Fatal(err)
	}
	machineThread(t, s, 1, nil, "ordinary")
	machineThread(t, s, 2, nil, "ordinary")
	p := PendingPlacement{Thread: 1, Agent: "fake", Data: `{"key":"accepted-start","input":"go"}`, CreatedAt: 100, Reason: "no capacity"}
	p2 := PendingPlacement{Thread: 2, Machine: "build", Agent: "fake", Data: `{"key":"explicit-start"}`, CreatedAt: 200}
	r := MachineReservation{Thread: 1, Machine: "build", Agent: "fake", Operation: "start-original", CreatedAt: 300}
	if err := s.Update(ctx, func(tx *Tx) error {
		if err := tx.PutPendingPlacement(ctx, p); err != nil {
			return err
		}
		if err := tx.PutPendingPlacement(ctx, p); err != nil {
			return err
		}
		if err := tx.PutPendingPlacement(ctx, p2); err != nil {
			return err
		}
		if err := tx.ReserveMachine(ctx, r); err != nil {
			return err
		}
		return tx.ReserveMachine(ctx, r)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ps, err := s.PendingPlacements(ctx); err != nil || !reflect.DeepEqual(ps, []PendingPlacement{p, p2}) {
		t.Fatalf("pending: %+v %v", ps, err)
	}
	if rs, err := s.MachineReservations(ctx); err != nil || !reflect.DeepEqual(rs, []MachineReservation{r}) {
		t.Fatalf("reservations: %+v %v", rs, err)
	}
	if err := s.Update(ctx, func(tx *Tx) error { return tx.FinishPlacement(ctx, 1, "stale-operation") }); err == nil {
		t.Fatal("stale reply consumed reservation")
	}
	changed := p
	changed.Data = "another start"
	if err := s.Update(ctx, func(tx *Tx) error { return tx.PutPendingPlacement(ctx, changed) }); err == nil {
		t.Fatal("accepted start replaced")
	}
	changedR := r
	changedR.Operation = "new-operation"
	if err := s.Update(ctx, func(tx *Tx) error { return tx.ReserveMachine(ctx, changedR) }); err == nil {
		t.Fatal("reservation replaced")
	}
	if err := s.Update(ctx, func(tx *Tx) error { return tx.FinishPlacement(ctx, 1, r.Operation) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM threads`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("reconciliation made a thread: %d %v", n, err)
	}
	if ps, err := s.PendingPlacements(ctx); err != nil || !reflect.DeepEqual(ps, []PendingPlacement{p2}) {
		t.Fatalf("another pending start changed: %+v %v", ps, err)
	}
	if rs, err := s.MachineReservations(ctx); err != nil || len(rs) != 0 {
		t.Fatalf("reconciled reservation retained: %+v %v", rs, err)
	}
}

func TestMachineConfinedAndReservationGuards(t *testing.T) {
	ctx := context.Background()
	s, _ := machineStore(t)
	if err := s.SyncMachines(ctx, []MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"supervisor", "conversation", "lookup"} {
		id := int64(i + 1)
		machineThread(t, s, id, "local", kind)
		if _, err := s.DB.Exec(`UPDATE threads SET machine='build' WHERE id=?`, id); err == nil {
			t.Fatal("confined thread moved")
		}
		if err := s.Update(ctx, func(tx *Tx) error {
			return tx.PutPendingPlacement(ctx, PendingPlacement{Thread: id, Agent: "fake", Data: "start"})
		}); err == nil {
			t.Fatal("confined thread queued for worker placement")
		}
		if _, err := s.DB.Exec(`INSERT INTO threads (scope,key,started_by,state,created_at,updated_at,machine,kind) VALUES ('s','remote','ada','creating',1,1,'build',?)`, kind); err == nil {
			t.Fatal("confined thread created remotely")
		}
	}
	machineThread(t, s, 10, "local", "ordinary")
	if err := s.Update(ctx, func(tx *Tx) error {
		return tx.PutPendingPlacement(ctx, PendingPlacement{Thread: 10, Machine: "build", Agent: "fake", Data: "wrong machine"})
	}); err == nil {
		t.Fatal("pending placement contradicted a placed thread's machine")
	}
	if err := s.Update(ctx, func(tx *Tx) error {
		return tx.ReserveMachine(ctx, MachineReservation{Thread: 10, Machine: "build", Agent: "fake", Operation: "wrong"})
	}); err == nil {
		t.Fatal("reservation for another machine accepted")
	}
	machineThread(t, s, 11, nil, "ordinary")
	if err := s.SyncMachines(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, func(tx *Tx) error {
		return tx.ReserveMachine(ctx, MachineReservation{Thread: 11, Machine: "build", Agent: "fake", Operation: "removed"})
	}); err == nil {
		t.Fatal("removed machine reserved")
	}
}

func TestMachineHealthOutageAndUsageKeys(t *testing.T) {
	ctx := context.Background()
	s, _ := machineStore(t)
	if err := s.SyncMachines(ctx, []MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
		t.Fatal(err)
	}
	for _, machine := range []string{"local", "build"} {
		for _, q := range []string{
			`INSERT INTO machine_health (machine,observed_at,boot) VALUES (?,100,'boot')`,
			`INSERT INTO machine_agent_health (machine,agent,config_dir,observed_at,state) VALUES (?,'fake','same-spelling',100,'ready')`,
			`INSERT INTO login_outages (machine,agent,config_dir,opened_at) VALUES (?,'fake','same-spelling',100)`,
			`INSERT INTO agent_limits (machine,agent,config_dir,at,every,windows) VALUES (?,'fake','same-spelling',100,1000,'[]')`,
		} {
			if _, err := s.DB.Exec(q, machine); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, table := range []string{"machine_health", "machine_agent_health", "login_outages", "agent_limits"} {
		var n int
		if err := s.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 2 {
			t.Fatalf("%s conflated machines: %d %v", table, n, err)
		}
	}
	var load sql.NullFloat64
	if err := s.DB.QueryRow(`SELECT load FROM machine_health WHERE machine='build'`).Scan(&load); err != nil || load.Valid {
		t.Fatalf("unknown load became zero: %+v %v", load, err)
	}
}

func TestMachineReservationHonorsAcceptedPlacement(t *testing.T) {
	for _, tc := range []struct{ name, machine, agent string }{
		{"explicit machine", "local", "fake"},
		{"accepted agent", "build", "another-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := machineStore(t)
			if err := s.SyncMachines(ctx, []MachineBinding{{Key: "build", SSH: "worker", Root: "/srv/worker"}}); err != nil {
				t.Fatal(err)
			}
			machineThread(t, s, 1, nil, "ordinary")
			p := PendingPlacement{Thread: 1, Machine: "build", Agent: "fake", Data: "accepted start"}
			if err := s.Update(ctx, func(tx *Tx) error { return tx.PutPendingPlacement(ctx, p) }); err != nil {
				t.Fatal(err)
			}
			r := MachineReservation{Thread: 1, Machine: tc.machine, Agent: tc.agent, Operation: "start"}
			if err := s.Update(ctx, func(tx *Tx) error { return tx.ReserveMachine(ctx, r) }); err == nil {
				t.Fatal("reservation changed the accepted request")
			}
			var machine sql.NullString
			if err := s.DB.QueryRow(`SELECT machine FROM threads WHERE id = 1`).Scan(&machine); err != nil || machine.Valid {
				t.Fatalf("refused reservation bound the thread: %+v %v", machine, err)
			}
			if rs, err := s.MachineReservations(ctx); err != nil || len(rs) != 0 {
				t.Fatalf("refused reservation persisted: %+v %v", rs, err)
			}
			if ps, err := s.PendingPlacements(ctx); err != nil || !reflect.DeepEqual(ps, []PendingPlacement{p}) {
				t.Fatalf("accepted request changed: %+v %v", ps, err)
			}
		})
	}
}
