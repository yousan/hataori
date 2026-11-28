package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestLimitObservationUpgradePreservesLegacyData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(newRoot(t), File)
	step := slices.IndexFunc(migrations, func(m Migration) bool { return m.Name == limitObservationsMigration.Name })
	if step < 0 {
		t.Fatal("missing migration")
	}
	s, err := open(ctx, path, migrations[:step])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at) VALUES(1,'s','a','owner','idle',0,0)`,
		`INSERT INTO sessions(id,thread_id,agent,state,last_seq,created_at,updated_at) VALUES(1,1,'fake','idle',7,0,0)`,
		`INSERT INTO agent_limits(machine,agent,config_dir,at,every,windows) VALUES('local','fake','a',1234,300000,'[{"name":"week","used":17}]')`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = open(ctx, path, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var at, every, report, seq int64
	var windows, obs, history string
	if err := s.DB.QueryRow(`SELECT at,every,windows,observations,report_at FROM agent_limits WHERE machine='local' AND agent='fake' AND config_dir='a'`).Scan(&at, &every, &windows, &obs, &report); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT last_seq,limit_observations FROM sessions WHERE id=1`).Scan(&seq, &history); err != nil {
		t.Fatal(err)
	}
	if at != 1234 || every != 300000 || report != 1234 || windows != `[{"name":"week","used":17}]` || obs != "[]" || seq != 7 || history != "" {
		t.Fatalf("upgrade changed legacy data: %d %d %d %q %q %d %q", at, every, report, windows, obs, seq, history)
	}
}
