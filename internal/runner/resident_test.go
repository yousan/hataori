package runner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
)

// Hold the cap while the fake agents and their known idle order are set
// up. Followers skip a cap pass in progress; their next pass exercises the
// same automatic enforcement used after a start or a daemon reconnect.
func holdResidentCap(t *testing.T, r *Runner, limit int) func() {
	t.Helper()
	r.MaxResidentAgents = limit // before the first follower starts
	r.residentMu.Lock()
	held := true
	release := func() {
		if held {
			held = false
			r.residentMu.Unlock()
		}
	}
	t.Cleanup(release)
	return release
}

func TestResidentCapPreservesPreparedTerminalAttach(t *testing.T) {
	f := newFixture(t, options{terminal: true})
	holdResidentCap(t, f.run, 1)
	residentStart(t, f, 1, "reply ready", "idle")
	residentStart(t, f, 2, "background 60s\nreply protected", "idle")
	api := f.core.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all | control.OpAttach})
	if _, err := api.AttachTerminal(context.Background(), control.ThreadID(1)); err != nil {
		t.Fatal(err)
	}
	// Let the clock move beyond AttachTerminal's millisecond. The cap
	// selects its candidate after the API call, before host.Attach.
	time.Sleep(2 * time.Millisecond)
	f.run.enforceResidentCap()
	if got := sessionState(t, f.st, 1); got != "idle" {
		t.Fatalf("terminal prepared but not connected yet: %s", got)
	}
	a, err := host.Attach(host.SocketPath(StateDir(f.root, 1)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	f.run.enforceResidentCap()
	if got := sessionState(t, f.st, 1); got != "idle" || !agentAlive(t, f.root, 1) {
		t.Fatalf("attached terminal: state %s", got)
	}
}

func residentStart(t *testing.T, f *fixture, id int64, script, state string) {
	t.Helper()
	_, err := owner(f.core).Start(context.Background(), control.StartRequest{
		Key: fmt.Sprint("resident-", id), Scope: "s", Thread: fmt.Sprint("resident-", id),
		Repo: "file:///unused", Input: control.Input{Text: script},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent ready in "+state, 15*time.Second, func() bool {
		return sessionState(t, f.st, id) == state
	})
}

func residentIdleAt(t *testing.T, f *fixture, id int64, at time.Time) {
	t.Helper()
	if _, err := f.st.DB.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, at.UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
}

func TestResidentCapSleepsLongestIdleFirst(t *testing.T) {
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := newFixture(t, options{})
			release := holdResidentCap(t, f.run, limit)
			for id := int64(1); id <= 3; id++ {
				residentStart(t, f, id, "reply ready", "idle")
			}
			// The idle order deliberately differs from thread/session IDs.
			now := time.Now()
			residentIdleAt(t, f, 3, now.Add(-3*time.Minute))
			residentIdleAt(t, f, 1, now.Add(-2*time.Minute))
			residentIdleAt(t, f, 2, now.Add(-time.Minute))
			release()
			if limit == 0 {
				time.Sleep(2200 * time.Millisecond) // two automatic cap passes
			} else {
				waitFor(t, "the oldest agent asleep", 10*time.Second, func() bool {
					return sessionState(t, f.st, 3) == "asleep"
				})
				if limit == 1 {
					waitFor(t, "the next oldest agent asleep", 10*time.Second, func() bool {
						return sessionState(t, f.st, 1) == "asleep"
					})
				}
			}
			for _, id := range []int64{1, 2, 3} {
				want := "idle"
				if limit > 0 && (id == 3 || limit == 1 && id == 1) {
					want = "asleep"
				}
				if got := sessionState(t, f.st, id); got != want {
					t.Fatalf("session %d: %s, want %s", id, got, want)
				}
			}
			if limit == 2 {
				waitFor(t, "the slept host stopped", 10*time.Second, func() bool { return host.Gone(StateDir(f.root, 3)) })
				send(t, owner(f.core), control.ThreadID(3), "again", "reply resumed")
				waitFor(t, "the resumed turn", 15*time.Second, f.has("turn_ended", 4))
				waitFor(t, "the newly oldest session asleep", 10*time.Second, func() bool {
					return sessionState(t, f.st, 1) == "asleep"
				})
				if got := sessionState(t, f.st, 3); got != "idle" {
					t.Fatalf("resumed session: %s", got)
				}
				var sessions, turns int
				if err := f.st.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
					t.Fatal(err)
				}
				if err := f.st.DB.QueryRow(`SELECT count(*) FROM turns`).Scan(&turns); err != nil {
					t.Fatal(err)
				}
				if sessions != 3 || turns != 4 {
					t.Fatalf("history: %d sessions, %d turns", sessions, turns)
				}
			}
		})
	}
}

func TestResidentCapPreservesProtectedAgents(t *testing.T) {
	for _, kind := range []string{"running", "question", "background", "failed background", "attached", "pending"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, options{terminal: kind == "attached", background: time.Hour})
			release := holdResidentCap(t, f.run, 1)
			script, state := "reply ready", "idle"
			switch kind {
			case "running":
				script, state = "tool wait 60s", "running"
			case "question":
				script, state = "ask which?", "waiting"
			case "background":
				script = "background 60s\nreply ready"
			case "failed background":
				script = "background 60s\nfail api_error"
			}
			residentStart(t, f, 1, script, state)
			if kind == "attached" {
				a, err := host.Attach(host.SocketPath(StateDir(f.root, 1)))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { a.Close() })
			}
			if kind == "pending" {
				if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, request, data, written) VALUES (10, 1, '', '{}', 1)`); err != nil {
					t.Fatal(err)
				}
			}
			residentStart(t, f, 2, "reply disposable", "idle")
			residentIdleAt(t, f, 1, time.Now().Add(-2*time.Minute))
			residentIdleAt(t, f, 2, time.Now().Add(-time.Minute))
			release()
			waitFor(t, "the eligible agent asleep", 10*time.Second, func() bool {
				return sessionState(t, f.st, 2) == "asleep"
			})
			if got := sessionState(t, f.st, 1); got != state || !agentAlive(t, f.root, 1) {
				t.Fatalf("protected session: state %s, agent alive %v", got, agentAlive(t, f.root, 1))
			}
		})
	}
}

func TestResidentCapWaitsForFailedBackgroundTasks(t *testing.T) {
	f := newFixture(t, options{background: 3 * time.Second})
	release := holdResidentCap(t, f.run, 1)
	residentStart(t, f, 1, "background 60s\nfail api_error", "idle")
	residentStart(t, f, 2, "background 60s\nreply ready", "idle")
	residentIdleAt(t, f, 1, time.Now())
	release()
	time.Sleep(1500 * time.Millisecond)
	if got := sessionState(t, f.st, 1); got != "idle" {
		t.Fatalf("before background wait: %s", got)
	}
	waitFor(t, "the failed turn's background wait", 10*time.Second, func() bool {
		return sessionState(t, f.st, 1) == "asleep"
	})
	if got := sessionState(t, f.st, 2); got != "idle" || !agentAlive(t, f.root, 2) {
		t.Fatalf("completed turn's background tasks: %s", got)
	}
}

func TestResidentCapSkipsNewActivity(t *testing.T) {
	f := newFixture(t, options{})
	release := holdResidentCap(t, f.run, 1)
	residentStart(t, f, 1, "reply old", "idle")
	residentStart(t, f, 2, "reply newer", "idle")
	old := time.Now().Add(-2 * time.Minute)
	residentIdleAt(t, f, 1, old)
	residentIdleAt(t, f, 2, time.Now().Add(-time.Minute))
	// An attach or another idle event updates the row after selection.
	residentIdleAt(t, f, 1, time.Now())
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	lv.mu.Lock()
	slept, err := f.run.sleepIfIdleAt(context.Background(), lv, old.UnixMilli())
	lv.mu.Unlock()
	if err != nil || slept {
		t.Fatalf("stale candidate: slept %v, err %v", slept, err)
	}
	release()
	waitFor(t, "the older remaining session asleep", 10*time.Second, func() bool {
		return sessionState(t, f.st, 2) == "asleep"
	})
	if got := sessionState(t, f.st, 1); got != "idle" {
		t.Fatalf("recently active session: %s", got)
	}
}
