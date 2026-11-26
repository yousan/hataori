package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/proctest"
	"github.com/yousan/hataori/internal/session"
)

// Rows marked stale by older runners must not reach the current process,
// even when a daemon reconnects directly to an already running agent.
func TestReconnectRejectsOldStaleInput(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply first")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.run.Close()
	data, _ := json.Marshal(session.PendingInput{Input: control.Input{Text: "reply old"}, Origin: &session.InputOrigin{Key: "m2", Text: "reply old"}})
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data, stale) VALUES (42, 1, ?, 1)`, string(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, request, data, stale) VALUES (43, 1, 'old-question', '{"text":"old answer"}', 1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st, c, r, err := build(f.root, options{}, f.log)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Reconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := withText(t, st, "input_failed", "reply old"); n != 1 {
			t.Errorf("reconnect %d: failure count %d, want 1", i, n)
		}
		if n := count(t, st, "pending_inputs"); n != 0 {
			t.Errorf("reconnect %d: %d old inputs left", i, n)
		}
		send(t, owner(c), thread, fmt.Sprint("new", i), "reply new")
		waitFor(t, "the next turn", 15*time.Second, func() bool { return withText(t, st, "text", "new") == i+1 })
		if n := withText(t, st, "text", "old"); n != 0 {
			t.Errorf("old input was delivered %d times", n)
		}
		r.Close()
		st.Close()
	}
}

func TestQueueRetirementRollsBackWithItsFailure(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply first")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.run.Close()
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data, context) VALUES (42, 1, '{"text":"reply old"}', 'owed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`CREATE TRIGGER refuse_failure BEFORE INSERT ON events WHEN NEW.kind = 'input_failed' BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.run.newAgent(context.Background(), 1); err == nil {
		t.Fatal("retired input without recording failure")
	}
	var stale int
	var earlier string
	if err := f.st.DB.QueryRow(`SELECT stale, context FROM pending_inputs WHERE id = 42`).Scan(&stale, &earlier); err != nil || stale != 0 || earlier != "owed" {
		t.Fatalf("rollback row: stale %d, context %q: %v", stale, earlier, err)
	}
	if _, err := f.st.DB.Exec(`DROP TRIGGER refuse_failure`); err != nil {
		t.Fatal(err)
	}
	if err := f.run.newAgent(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if n := withText(t, f.st, "input_failed", "reply old"); n != 1 {
		t.Fatalf("failure count %d, want 1", n)
	}
	if n := count(t, f.st, "pending_inputs"); n != 0 {
		t.Fatalf("%d old inputs left", n)
	}
}

// The call can be accepted for the old process, paused before dispatch,
// then released after the replacement's codec has become current.
func TestRetiredInputCannotBeDispatchedToTheNextAgent(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply first")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.run.Close()
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, data) VALUES (42, 1, '{"text":"old input"}')`); err != nil {
		t.Fatal(err)
	}
	if err := f.run.newAgent(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	codec := &handed{got: map[string]agent.Input{}}
	r := &Runner{Store: f.st, live: map[int64]*live{1: {id: 1, codec: codec}}}
	if err := r.Deliver(context.Background(), 1, 42, control.Input{Text: "old input"}); err != nil {
		t.Fatal(err)
	}
	if len(codec.got) != 0 {
		t.Fatalf("replacement was given retired input: %v", codec.got)
	}
}

func TestDelayedDispatchCannotOutliveItsFollower(t *testing.T) {
	rec := &recorder{inputs: map[string][]string{}}
	f := newFixture(t, options{wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	selected, release := make(chan struct{}), make(chan struct{})
	f.run.point = func(point string) {
		if point == "before pending dispatch" {
			close(selected)
			<-release
		}
	}
	f.start("reply first")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.core.Runner = lostDelivery{f.run}
	send(t, owner(f.core), thread, "old", "reply old")
	f.core.Runner = f.run
	var pending int64
	if err := f.st.DB.QueryRow(`SELECT max(id) FROM pending_inputs`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.run.Deliver(context.Background(), 1, pending, control.Input{Text: "reply old"}) }()
	<-selected
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	}()
	proctest.Kill(filepath.Join(StateDir(f.root, 1), host.PIDFile))
	waitFor(t, "the old follower's end", 15*time.Second, func() bool {
		_, err := f.run.get(1)
		return err != nil && sessionState(t, f.st, 1) == "asleep"
	})
	send(t, owner(f.core), thread, "new", "reply new")
	waitFor(t, "the replacement's turn", 15*time.Second, f.has("text", 2))
	close(release)
	// The deferred join ensures no old action is still pending when
	// fixtures stop; wait here so the assertions see the completed call.
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		done <- nil
	case <-time.After(15 * time.Second):
		t.Fatal("delayed delivery did not return")
	}
	for _, in := range rec.of(f.root, 1) {
		if in == "reply old" {
			t.Fatalf("old codec received retired input: %q", rec.of(f.root, 1))
		}
	}
	if n := withText(t, f.st, "text", "old"); n != 0 {
		t.Fatalf("old input reached the replacement %d times", n)
	}
	if n := withText(t, f.st, "input_failed", "reply old"); n != 1 {
		t.Fatalf("failures %d, want 1", n)
	}
}
