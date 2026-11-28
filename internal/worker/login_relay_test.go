package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

func TestWorkerLoginOwnerOutageAndPersistence(t *testing.T) {
	for _, useDefault := range []bool{false, true} {
		name := "explicit"
		if useDefault {
			name = "default"
		}
		t.Run(name, func(t *testing.T) { testWorkerLoginOwnerOutageAndPersistence(t, useDefault, false) })
	}
}

func TestWorkerLoginFakeSurfacePersistence(t *testing.T) {
	testWorkerLoginOwnerOutageAndPersistence(t, false, true)
}

func testWorkerLoginOwnerOutageAndPersistence(t *testing.T, useDefault, useSurface bool) {
	f, adapter, open, count := loginResidentDir(t, "code", useDefault)
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
	var driver *fake.Driver
	var dm surface.Conversation
	var selected surface.MessageRef
	stopSurface := func() {}
	if useSurface {
		adapter.extraEnv = []string{"WORKER_LOGIN_ARGV_CAPTURE=" + filepath.Join(f.root, "argv-capture.jsonl")}
		driver = fake.New()
		lg := slog.New(slog.NewTextHandler(&diagnostic, nil))
		c := &core.Core{Store: st, People: r.People, Log: lg}
		fe := &chat.FrontEnd{Name: "chat", Instance: "fixture", Driver: driver, People: r.People, Relay: r, Log: lg, API: c.As,
			Outbox: &chat.Outbox{Store: st, Client: "chat", Driver: driver, Log: lg}}
		for _, id := range []int64{outage, other} {
			if _, err := st.DB.ExecContext(ctx, `INSERT INTO chat_logins(client,outage,agent,threads) VALUES('chat',?,'fake','[]')`, id); err != nil {
				t.Fatal(err)
			}
		}
		r.Notify = func(u relay.Update) { fe.LoginUpdate(u); updates <- u }
		surfaceCtx, surfaceCancel := context.WithCancel(ctx)
		surfaceDone := make(chan error, 1)
		go func() { surfaceDone <- fe.Run(surfaceCtx) }()
		stopSurface = func() {
			surfaceCancel()
			if err := <-surfaceDone; err != nil {
				t.Error(err)
			}
		}
		defer func() { stopSurface() }()
		dm, err = driver.OpenDirect(ctx, "owner")
		if err != nil {
			t.Fatal(err)
		}
		for selected.ID == "" || !driver.Connected() {
			if ctx.Err() != nil {
				t.Fatal("fake surface did not show the original outage")
			}
			for _, call := range driver.Calls(dm) {
				if call.Op == fake.OpPost && call.Key == fmt.Sprintf("login.%d", outage) {
					selected = call.Ref
				}
			}
			time.Sleep(time.Millisecond)
		}
	}
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
	if _, err := st.DB.ExecContext(ctx, `UPDATE machines SET installation='another-installation' WHERE key='build'`); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx, "chat:owner", "fake", outage); err == nil || count.Load() != 0 {
		t.Fatal("unpinned installation reached transport")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE machines SET installation=? WHERE key='build'`, f.binding.Worker); err != nil {
		t.Fatal(err)
	}
	if useSurface {
		var token string
		for _, call := range driver.Calls(dm) {
			if call.Ref == selected && len(call.Message.Choices) == 1 {
				token = call.Message.Choices[0].Token
			}
		}
		if err := driver.Press(selected, "someone", token); err != nil || count.Load() != 0 {
			t.Fatal("guest button reached the worker login transport")
		}
		if err := driver.Press(selected, "owner", token); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := r.Start(ctx, "chat:owner", "fake", outage); err != nil {
			t.Fatal(err)
		}
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
	for _, from := range []string{"chat:guest", "chat:someone"} {
		if err := r.Code(ctx, from, "fake", outage, "marker-secret"); !errors.Is(err, relay.ErrForbidden) {
			t.Fatal("non-owner sent a code to the waiting command")
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
	if useSurface {
		// Wait for the real front end to show the worker's link, then use
		// the fake driver's normal inbound delivery, including ReplyTo.
		shown := false
		for !shown {
			for _, call := range driver.Calls(dm) {
				shown = shown || strings.Contains(call.Message.Text, "https://login.example.invalid/")
			}
			if ctx.Err() != nil {
				t.Fatal("worker link did not reach the owner's direct message")
			}
			time.Sleep(time.Millisecond)
		}
		driver.InjectWith(dm, fake.Person("someone"), "marker-secret", func(in *surface.Inbound) { in.Direct = true; in.ReplyTo = &selected })
		driver.InjectWith(dm, fake.Person("owner"), "marker-secret", func(in *surface.Inbound) { in.Direct = true; in.ReplyTo = &selected })
	} else {
		if err := r.Code(ctx, "chat:owner", "fake", outage, "marker-secret"); err != nil {
			t.Fatal(err)
		}
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
	stopSurface()
	stopSurface = func() {}
	if useSurface {
		b, err := os.ReadFile(filepath.Join(f.root, "argv-capture.jsonl"))
		if err != nil {
			t.Fatal("guard and CLI argument observations are missing")
		}
		rows := bytes.Split(bytes.TrimSpace(b), []byte{'\n'})
		if len(rows) != 4 {
			t.Fatalf("observed %d process arguments, want two guards, login and status", len(rows))
		}
		for _, row := range rows {
			var argv []string
			if json.Unmarshal(row, &argv) != nil || len(argv) < 4 {
				t.Fatal("invalid process argument observation")
			}
		}
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
