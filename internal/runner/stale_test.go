package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// A launch records what the config gave it, and shows its model where the
// config's changed. One that a daemon that died had begun may find the host
// that daemon started, with what it was given then: the record of that
// stays, so that the session still counts as launched with another config.
func TestLaunchRecordsWhatItWasGiven(t *testing.T) {
	f := newFixture(t, options{confines: true})
	model, effort, part := "m-1", "low", "x"
	f.run.ConfinedConfig = func(scope, agent string) (LaunchConfig, bool) {
		return LaunchConfig{Model: model, Effort: effort, Parts: map[string]string{"a part": part}}, scope == "s" && agent == "fake"
	}
	if _, err := f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "k", Scope: "s", Thread: "t", Input: control.Input{Text: "reply hello"}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn", 15*time.Second, f.has("turn_ended", 1))
	stored := func() (s storedSpec, column string) {
		t.Helper()
		var raw string
		if err := f.st.DB.QueryRow(`SELECT spec, model FROM sessions WHERE id = 1`).Scan(&raw, &column); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		return s, column
	}
	s, column := stored()
	if s.Config == nil || s.Config.Model != "m-1" || s.Config.Parts["a part"] != "x" || s.Spec.Model != "m-1" || column != "m-1" {
		t.Fatalf("stored %+v, model %q; want what the launch was given", s, column)
	}
	lv := &live{id: 1, spec: s.Spec}
	if err := f.run.compare(lv); err != nil || lv.stale {
		t.Fatalf("stale %v, %v, with the config it was launched with", lv.stale, err)
	}

	f.run.Close()
	// Another launch, given another part, leaves the model the agent
	// reported as it is, also after a hataori that recorded nothing.
	if _, err := f.st.DB.Exec(`UPDATE sessions SET model = 'm-1-reported' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"y", "z", "another effort"} {
		killSessions(f.root)
		waitFor(t, "the host's end", 10*time.Second, func() bool { return len(hosts(t, StateDir(f.root, 1))) == 0 })
		part = p
		if p == "another effort" {
			effort = "max"
		}
		if _, err := f.run.launch(context.Background(), 1, "fake", f.run.Adapters["fake"], s.Spec, firstInput{NoInput: true}); err != nil {
			t.Fatal(err)
		}
		if s, column = stored(); s.Config == nil || s.Config.Parts["a part"] != p || column != "m-1-reported" {
			t.Fatalf("stored %+v, model %q; want the part %q, and the model the agent reported", s, column, p)
		}
		var e string
		if err := f.st.DB.QueryRow(`SELECT effort FROM sessions WHERE id = 1`).Scan(&e); err != nil || e != effort {
			t.Fatalf("%s: effort %q, %v; want %q", p, e, err, effort)
		}
		if p == "y" {
			if _, err := f.st.DB.Exec(`UPDATE sessions SET spec = json_remove(spec, '$.config') WHERE id = 1`); err != nil {
				t.Fatal(err)
			}
		}
	}

	model = "m-2"
	if _, err := f.st.DB.Exec(`UPDATE sessions SET launch = 2 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run.launch(context.Background(), 1, "fake", f.run.Adapters["fake"], s.Spec, firstInput{NoInput: true}); err != nil {
		t.Fatal(err)
	}
	if n := len(hosts(t, StateDir(f.root, 1))); n != 1 {
		t.Fatalf("%d hosts, want the one that ran", n)
	}
	if s, column = stored(); s.Config == nil || s.Config.Model != "m-1" || column != "m-1-reported" {
		t.Fatalf("stored %+v, model %q; want what the running host was given", s, column)
	}
	lv = &live{id: 1, spec: s.Spec}
	if err := f.run.compare(lv); err != nil || !lv.stale || !strings.Contains(f.log.String(), "changed=model") {
		t.Fatalf("stale %v, %v; want it stale by the model; the log:\n%s", lv.stale, err, f.log.String())
	}
}

// Reconnect does not put a session launched with another config to sleep
// while a request of its agent is open; it sleeps once that is closed.
// Otherwise it is done only once an idle session launched with another config
// sleeps, also while the store is busy: the message after it finds the
// session asleep, and is launched anew.
func TestReconnectPutsAStaleSessionToSleep(t *testing.T) {
	t.Run("idle", func(t *testing.T) { reconnectStale(t, false) })
	t.Run("a request open", func(t *testing.T) { reconnectStale(t, true) })
}

func reconnectStale(t *testing.T, request bool) {
	o := options{confines: true, conversations: t.TempDir()}
	f := newFixture(t, o)
	config := func(model string) func(string, string) (LaunchConfig, bool) {
		return func(string, string) (LaunchConfig, bool) { return LaunchConfig{Model: model}, true }
	}
	f.run.ConfinedConfig = config("m-1")
	if _, err := f.core.StartConfined(context.Background(), control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all},
		control.StartRequest{Key: "k", Scope: "s", Thread: "t", Input: control.Input{Text: "reply hello"}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn", 15*time.Second, f.has("turn_ended", 1))
	f.run.Close()

	if request {
		if _, err := f.st.DB.Exec(`INSERT INTO requests (session_id, request, handle, approval, text, choices, state, opened_at)
			VALUES (1, 'r', 'h', 0, 'Which?', '[]', 'open', 1)`); err != nil {
			t.Fatal(err)
		}
	}
	st, _, r, err := build(f.root, o, f.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); st.Close() })
	r.ConfinedConfig = config("m-2")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.root, store.File)+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(700*time.Millisecond, func() { tx.Rollback() })
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := func() (state string) {
		if err := st.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	if request {
		time.Sleep(2 * staleCheck)
		if got := state(); got != string(control.Idle) {
			t.Fatalf("the session is %s with a request open, want it idle", got)
		}
		if _, err := st.DB.Exec(`UPDATE requests SET state = 'answered', closed_at = 2`); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "its sleep", 5*time.Second, func() bool { return state() == string(control.Asleep) })
		return
	}
	if got := state(); got != string(control.Asleep) {
		t.Fatalf("the session is %s once Reconnect is done, want it asleep; the log:\n%s", got, f.log.String())
	}
}

// A stale session that is idle with a first input its agent has not
// reported yet, as after a turn the agent started on its own, is not put to
// sleep before that input is reported.
func TestStaleSessionKeepsAnUnreportedFirstInput(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	waitFor(t, "the turn", 15*time.Second, f.has("turn_ended", 1))
	f.run.mu.Lock()
	lv := f.run.live[1]
	f.run.mu.Unlock()
	lv.mu.Lock()
	defer lv.mu.Unlock()
	lv.stale = true
	for _, c := range []struct {
		first string
		want  control.State
	}{{`{"sent":true}`, control.Idle}, {"", control.Asleep}} {
		if _, err := f.st.DB.Exec(`UPDATE sessions SET first_input = ? WHERE id = 1`, c.first); err != nil {
			t.Fatal(err)
		}
		f.run.checkStale(lv, true)
		var state string
		if err := f.st.DB.QueryRow(`SELECT state FROM sessions WHERE id = 1`).Scan(&state); err != nil || state != string(c.want) {
			t.Fatalf("with the first input %q the session is %s, %v; want it %s", c.first, state, err, c.want)
		}
	}
}
