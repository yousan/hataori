package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

func limitMachine(t *testing.T) *Machine {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at) VALUES(1,'s','a','owner','idle',0,0),(2,'s','b','owner','idle',0,0)`,
		`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(1,1,'fake','idle',0,0),(2,2,'fake','idle',0,0)`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return &Machine{Store: s}
}

func limitInput(seq uint64, minute, used int, reset time.Time, cached bool) Input {
	return Input{Kind: AgentEvent, Agent: agent.Event{Seq: seq, Kind: agent.Limits, Limits: &agent.LimitReport{
		At: time.Unix(10000, 0).Add(time.Duration(minute) * time.Minute), Every: 5 * time.Minute, Cached: cached,
		Windows: []agent.LimitWindow{{Name: "week", Minutes: 10080, Used: used, Resets: reset}},
	}}}
}

func keptLimit(t *testing.T, m *Machine, machine, dir string) (int, int64) {
	t.Helper()
	var at int64
	var raw string
	if err := m.Store.DB.QueryRow(`SELECT at,windows FROM agent_limits WHERE machine=? AND agent='fake' AND config_dir=?`, machine, dir).Scan(&at, &raw); err != nil {
		t.Fatal(err)
	}
	var ws []control.LimitWindow
	if err := json.Unmarshal([]byte(raw), &ws); err != nil || len(ws) != 1 {
		t.Fatalf("windows: %s %v", raw, err)
	}
	return ws[0].Used, at
}

func TestCachedLimitReceiptOrderAndFreshReads(t *testing.T) {
	ctx := context.Background()
	m := limitMachine(t)
	reset := time.Unix(1000000, 0)
	for _, x := range []struct {
		sid int64
		in  Input
	}{
		{1, limitInput(1, 10, 10, reset, true)},
		{2, limitInput(1, 15, 20, reset, true)},
		{1, limitInput(2, 30, 10, reset, true)}, // receipt advances, observation does not
		{1, limitInput(3, 20, 15, reset, true)}, // delayed changed value must not rewind history
		{1, limitInput(4, 40, 10, reset, true)},
	} {
		if err := m.Apply(ctx, x.sid, x.in); err != nil {
			t.Fatal(err)
		}
	}
	if used, at := keptLimit(t, m, "local", ""); used != 20 || at != limitInput(1, 15, 0, reset, true).Agent.Limits.At.UnixMilli() {
		t.Fatalf("delayed cache replaced newer source: %d at %d", used, at)
	}
	if err := m.Apply(ctx, 1, limitInput(5, 41, 10, reset.Add(time.Second), true)); err != nil {
		t.Fatal(err)
	}
	if used, _ := keptLimit(t, m, "local", ""); used != 20 {
		t.Fatalf("cached reset jitter renewed old value: %d", used)
	}
	shorter := limitInput(2, 42, 20, reset, true)
	shorter.Agent.Limits.Every = time.Minute
	if err := m.Apply(ctx, 2, shorter); err != nil {
		t.Fatal(err)
	}
	var every int64
	if err := m.Store.DB.QueryRow(`SELECT every FROM agent_limits`).Scan(&every); err != nil {
		t.Fatal(err)
	}
	if _, at := keptLimit(t, m, "local", ""); at != limitInput(1, 15, 0, reset, true).Agent.Limits.At.UnixMilli() || every != time.Minute.Milliseconds() {
		t.Fatalf("cached cadence did not stay conservative: at=%d every=%d", at, every)
	}
	// A captured-read source is fresh even when its normalized value repeats.
	if err := m.Apply(ctx, 2, limitInput(3, 50, 20, reset, false)); err != nil {
		t.Fatal(err)
	}
	if _, at := keptLimit(t, m, "local", ""); at != limitInput(1, 50, 0, reset, false).Agent.Limits.At.UnixMilli() {
		t.Fatalf("fresh read did not renew age: %d", at)
	}
	// Reset jitter must not let an older read bypass observation ordering.
	if err := m.Apply(ctx, 1, limitInput(6, 45, 99, reset.Add(time.Second), false)); err != nil {
		t.Fatal(err)
	}
	if used, _ := keptLimit(t, m, "local", ""); used != 20 {
		t.Fatalf("reset jitter replaced fresh value: %d", used)
	}
	// A genuinely new decrease, within that same period, is accepted.
	if err := m.Apply(ctx, 1, limitInput(7, 60, 19, reset.Add(time.Second), false)); err != nil {
		t.Fatal(err)
	}
	if used, _ := keptLimit(t, m, "local", ""); used != 19 {
		t.Fatalf("genuine decrease lost: %d", used)
	}
}

func TestLimitLegacyAgeAndTrustedBinding(t *testing.T) {
	ctx := context.Background()
	m := limitMachine(t)
	dir := "a"
	m.LoginDir = func(string, agent.Spec) string { return dir }
	reset := time.Unix(1000000, 0)
	in := limitInput(1, 0, 10, reset, true)
	w := control.LimitWindow{Name: "week", Minutes: 10080, Used: 10, Resets: &reset}
	b, _ := json.Marshal([]control.LimitWindow{w})
	if _, err := m.Store.DB.Exec(`INSERT INTO agent_limits(machine,agent,config_dir,at,every,windows,report_at) VALUES('local','fake','a',?,300000,?,?)`, in.Agent.Limits.At.UnixMilli(), string(b), in.Agent.Limits.At.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(ctx, 1, limitInput(1, 10, 10, reset, true)); err != nil {
		t.Fatal(err)
	}
	if _, at := keptLimit(t, m, "local", "a"); at != in.Agent.Limits.At.UnixMilli() {
		t.Fatalf("legacy age was renewed: %d", at)
	}
	dir = "b"
	if err := m.Apply(ctx, 1, limitInput(2, 20, 10, reset, true)); err != nil {
		t.Fatal(err)
	}
	if _, at := keptLimit(t, m, "local", "b"); at != limitInput(1, 20, 0, reset, true).Agent.Limits.At.UnixMilli() {
		t.Fatalf("directory reused foreign provenance: %d", at)
	}
	if err := m.Store.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
		t.Fatal(err)
	}
	// Placement is immutable. Even a copied private history must not cross
	// into a distinct daemon-bound session on another machine.
	if _, err := m.Store.DB.Exec(`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(3,'s','remote','owner','idle',0,0,'build')`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.DB.Exec(`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at,spec,limit_observations) SELECT 3,3,'fake','idle',0,0,'{"spec":{"ConfigDir":"b"}}',limit_observations FROM sessions WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(ctx, 3, limitInput(1, 30, 10, reset, true)); err != nil {
		t.Fatal(err)
	}
	if _, at := keptLimit(t, m, "build", "b"); at != limitInput(1, 30, 0, reset, true).Agent.Limits.At.UnixMilli() {
		t.Fatalf("machine reused foreign provenance: %d", at)
	}
}

func TestLimitEncodingFailureRollsBackProvenanceAndSequence(t *testing.T) {
	m := limitMachine(t)
	in := limitInput(1, 1, 10, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), true)
	if err := m.Apply(context.Background(), 1, in); err == nil {
		t.Fatal("unencodable reset accepted")
	}
	var seq int
	var history string
	if err := m.Store.DB.QueryRow(`SELECT last_seq,limit_observations FROM sessions WHERE id=1`).Scan(&seq, &history); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := m.Store.DB.QueryRow(`SELECT count(*) FROM agent_limits`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if seq != 0 || history != "" || n != 0 {
		t.Fatalf("failed report committed: seq=%d history=%q rows=%d", seq, history, n)
	}
}
