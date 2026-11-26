package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

func TestStoredScopeHasNoOwnThreadOrForge(t *testing.T) {
	ctx := context.Background()
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	s.Scopes["c1"] = Scope{ConversationsOnly: true, Conversations: true, Autonomy: Watch, Limits: Limits{Turns: 6, Sends: 5, Tokens: 500000}, RemindersMax: 3}
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	if id, err := s.StartOne(ctx, "c1"); id != 0 || !errors.Is(err, ErrNoChat) {
		t.Fatalf("%d %v", id, err)
	}
	s.Start(ctx) // must skip it, including the earlier own thread
	var n int
	s.Core.Store.DB.QueryRow(`SELECT count(*) FROM supervisor_dirs`).Scan(&n)
	if n != 0 {
		t.Fatal("own startup chose a directory")
	}
	api := s.API("c1", older)
	if _, err := api.Start(ctx, control.StartRequest{Scope: "c1", Key: "start", Repo: "https://example.com/o/listed"}); !errors.Is(err, control.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := api.Forge(ctx, control.ForgeRequest{Repo: "o/listed", Read: control.ForgeReadIssue, Number: 1}); !errors.Is(err, control.ErrForbidden) {
		t.Fatal(err)
	}
}

func TestStoredScopeDropsStaleWakeOnceAcrossReload(t *testing.T) {
	ctx := context.Background()
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	s.Core.Log = s.Log
	s.Core.Scopes = map[string]core.Scope{"c1": {Enabled: true, Unlisted: true}}
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	if err := Begin(ctx, s.Core, "c1"); err != nil {
		t.Fatal(err)
	}
	exec(t, s.Core, `INSERT INTO events(pos,scope,thread,thread_id,time,kind,data) VALUES (10,'c1','',2,0,'turn_ended','{"by":"caller","outcome":"completion","background":1,"session":3,"turn":7}')`)
	w := &Waker{Core: s.Core, Scope: "c1", Log: s.Log, Clock: at(time.Unix(0, 0).Add(BackgroundWait + time.Hour))}
	r := newWaking(w)
	r.pos = 10
	r.read = 10
	if err := r.loadWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.stale) != 1 {
		t.Fatalf("stale: %+v", r.stale)
	}
	if err := r.attempt(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.stale) != 0 || len(r.waiting) != 0 || !r.empty() {
		t.Fatal("dropped wake stayed pending")
	}
	if err := r.attempt(ctx); err != nil {
		t.Fatal(err)
	}
	again := newWaking(w)
	if err := again.load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := again.loadWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if len(again.stale) != 0 || len(again.waiting) != 0 {
		t.Fatal("dropped wake reloaded")
	}
	if strings.Count(log.String(), "wake dropped") != 1 {
		t.Fatal(log.String())
	}
	var dropped int
	if err := s.Core.Store.DB.QueryRow(`SELECT dropped FROM supervisor_wakes WHERE scope='c1'`).Scan(&dropped); err != nil || dropped != 10 {
		t.Fatalf("%d %v", dropped, err)
	}
}
