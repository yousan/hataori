package worker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/store"
)

func TestWorkerLoginOwnerOutageAndPersistence(t *testing.T) {
	for _, useDefault := range []bool{false, true} {
		name := "explicit"
		if useDefault {
			name = "default"
		}
		t.Run(name, func(t *testing.T) { testWorkerLoginOwnerOutageAndPersistence(t, useDefault) })
	}
}

func testWorkerLoginOwnerOutageAndPersistence(t *testing.T, useDefault bool) {
	f, _, open, count := loginResidentDir(t, "code", useDefault)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture/build"}, {Key: "other", SSH: "fixture-other", Root: "/fixture/other"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindMachineInstallation(ctx, "build", f.binding.Worker); err != nil {
		t.Fatal(err)
	}
	dir := f.options.Config.Agents[0].ConfigDir
	for _, machine := range []string{"local", "build", "other"} {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO login_outages(machine,agent,config_dir,opened_at) VALUES (?,'fake',?,1)`, machine, dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','key','owner','idle',1,1,'build')`); err != nil {
		t.Fatal(err)
	}
	spec := string(testJSON(t, struct {
		Spec agent.Spec `json:"spec"`
	}{Spec: agent.Spec{ConfigDir: dir}}))
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at,spec) VALUES(1,1,'fake','idle',1,1,?)`, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO session_machine_runs(session_id,machine,run,workspace,agent_config,phase) VALUES(1,'build','run-fixture','','default','running')`); err != nil {
		t.Fatal(err)
	}
	var outage, other int64
	if err := st.DB.QueryRowContext(ctx, `SELECT id FROM login_outages WHERE machine='build'`).Scan(&outage); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT id FROM login_outages WHERE machine='other'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO login_outage_threads(outage_id,thread_id) VALUES(?,1)`, outage); err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	updates := make(chan relay.Update, 16) // isolated stand-in for the owner's DM sink
	r := &relay.Relay{Store: st, People: people.New([]people.Person{{Name: "owner", IDs: []string{"chat:owner"}}, {Name: "someone", IDs: []string{"chat:someone"}}}, "owner", nil),
		Log: slog.New(slog.NewTextHandler(&diagnostic, nil)), Notify: func(u relay.Update) { updates <- u },
		Workers: map[string]relay.WorkerLoginEndpoint{"build": {Binding: f.binding, Open: open}}}
	for _, from := range []string{"chat:guest", "chat:someone"} {
		if err := r.Start(ctx, from, "fake", outage); err == nil {
			t.Fatal("non-owner started worker login")
		}
		if err := r.Code(ctx, from, "fake", outage, "marker-secret"); err == nil {
			t.Fatal("non-owner sent worker input")
		}
	}
	if count.Load() != 0 {
		t.Fatal("unauthorized request reached transport")
	}
	if err := r.Start(ctx, "chat:owner", "fake", outage); err != nil {
		t.Fatal(err)
	}
	for links := 0; links < 2; {
		select {
		case u := <-updates:
			if u.Outage != outage {
				t.Fatal("update reached another outage")
			}
			if u.Link != "" {
				links++
			}
		case <-ctx.Done():
			t.Fatal("worker did not request a code")
		}
	}
	if err := r.Code(ctx, "chat:owner", "fake", other, "marker-secret"); err == nil {
		t.Fatal("wrong outage accepted code")
	}
	if err := r.Code(ctx, "chat:owner", "another", outage, "marker-secret"); err == nil {
		t.Fatal("wrong agent accepted code")
	}
	// Simulate a restored row before its asynchronous event reader runs.
	if _, err := st.DB.ExecContext(ctx, `UPDATE login_outages SET closed_at=2 WHERE id=?`, outage); err != nil {
		t.Fatal(err)
	}
	if err := r.Code(ctx, "chat:owner", "fake", outage, "marker-secret"); err == nil {
		t.Fatal("closed outage accepted input before event cleanup")
	}
	// Restore the isolated snapshot so the successful-close path can finish.
	if _, err := st.DB.ExecContext(ctx, `UPDATE login_outages SET closed_at=NULL WHERE id=?`, outage); err != nil {
		t.Fatal(err)
	}
	if err := r.Code(ctx, "chat:owner", "fake", outage, "marker-secret"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case u := <-updates:
			if u.Done == "" {
				continue
			}
			if u.Done != relay.LoggedIn || u.Outage != outage {
				t.Fatalf("unexpected outcome: %s", u.Done)
			}
		case <-ctx.Done():
			t.Fatal("login did not finish")
		}
		break
	}
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT count(*) FROM login_outages WHERE closed_at IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("another machine's outage closed: %d %v", n, err)
	}
	if count.Load() != 1 {
		t.Fatal("login transport was replayed")
	}
	if bytes.Contains(diagnostic.Bytes(), []byte("marker-secret")) || bytes.Contains(diagnostic.Bytes(), []byte("https://login.example.invalid/")) {
		t.Fatal("diagnostic contains login material")
	}
	assertLoginMarkerAbsent(t, root)
	assertLoginMarkerAbsent(t, f.root)
	// A stale owner reply reaches no command after success.
	if err := r.Code(ctx, "chat:owner", "fake", outage, "marker-secret"); err == nil {
		t.Fatal("completed outage accepted input")
	}
}
