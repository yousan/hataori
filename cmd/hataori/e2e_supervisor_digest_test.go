package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #673: a digest gives each thread's output
// since the last digest that gave it to this session of the supervisor,
// not its last output again. End to end with the fake agent.

// again sends text to worker id as the owner, and waits for its next turn
// end, the n-th.
func (wt *wakeTest) again(t *testing.T, owner *wire.Client, id control.ThreadID, key, text string, n int) {
	t.Helper()
	if err := owner.Send(context.Background(), id, key, control.Input{Text: text}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	wt.read(t, wt.ended(t, id, n))
}

// Two turn ends of one thread, in two wakes: the second digest has only
// the second turn's output. A turn end with no output since gets one line.
func TestDigestGivesTheOutputSinceTheLast(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	defer stop()
	w, pos := wt.worker(t, owner, "c1", "w", "reply first part")
	wt.read(t, pos)
	if d := wt.wokenN(t, owner, sv.ID, 1)[0]; !strings.Contains(d, "Last output:\n> first part") {
		t.Fatalf("the first digest:\n%s", d)
	}
	wt.again(t, owner, w, "w-2", "reply second part", 2)
	d := wt.wokenN(t, owner, sv.ID, 2)[1]
	if !strings.Contains(d, "Output since the last digest:\n> second part") || strings.Contains(d, "> first part") {
		t.Fatalf("the second digest:\n%s", d)
	}

	// An interrupted turn writes no output.
	if err := owner.Send(context.Background(), w, "w-3", control.Input{Text: "tool wait 30s"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the tool", 15*time.Second, func() bool {
		var n int
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'tool_started'`, int64(w)).Scan(&n)
		return n == 1
	})
	if err := owner.Stop(context.Background(), w, 0); err != nil {
		t.Fatal(err)
	}
	wt.read(t, wt.ended(t, w, 3))
	if d := wt.wokenN(t, owner, sv.ID, 3)[2]; !strings.Contains(d, "No output since the last digest.") || strings.Contains(d, "> ") {
		t.Fatalf("the digest of a turn with no output:\n%s", d)
	}
}

// After a restart the next digest goes on from what was last given:
// nothing given again, nothing left out, also output of two turns since.
func TestDigestAcrossARestart(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, sv := wt.start(t)
	w, pos := wt.worker(t, owner, "c1", "w", "reply given before")
	wt.read(t, pos)
	wt.wokenN(t, owner, sv.ID, 1)
	wt.again(t, owner, w, "w-2", "reply b output", 2)
	stop()
	stop, owner, sv = wt.start(t)
	defer stop()
	wt.again(t, owner, w, "w-3", "reply c output", 3)
	d := wt.wokenN(t, owner, sv.ID, 2)[1]
	if !strings.Contains(d, "Output since the last digest:") || !strings.Contains(d, "> b output") || !strings.Contains(d, "> c output") ||
		strings.Contains(d, "> given before") {
		t.Fatalf("the digest after the restart:\n%s", d)
	}
}

// A new session of the supervisor, here its new thread after a person
// closed the last, gets the last output in its first digest, as before.
func TestDigestOfANewSupervisorSession(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, first := wt.start(t)
	w, pos := wt.worker(t, owner, "c1", "w", "reply given to the first")
	wt.read(t, pos)
	wt.wokenN(t, owner, first.ID, 1)
	if err := owner.CloseThread(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	stop()
	stop, owner, sv := wt.start(t)
	defer stop()
	if sv.ID == first.ID {
		t.Fatalf("the closed supervisor %d was not replaced", first.ID)
	}
	wt.again(t, owner, w, "w-2", "reply for the second", 2)
	d := wt.wokenN(t, owner, sv.ID, 1)[0]
	if !strings.Contains(d, "Last output:") || !strings.Contains(d, "> given to the first") || !strings.Contains(d, "> for the second") ||
		strings.Contains(d, "since the last digest") {
		t.Fatalf("the new session's first digest:\n%s", d)
	}
}

// A session of the supervisor's thread that replaced one that could not
// resume is new too: the wake carried into it was made for the old one,
// and the next digest gives the last output again.
func TestDigestAfterASessionThatCouldNotResume(t *testing.T) {
	// The real clock, which the idle period goes by, with a short idle
	// period and quiet period.
	repo := bareRepo(t)
	r, conv := svRoot(t, true, budget(0, 0, 0), func(cfg map[string]any) {
		cfg["idle"] = "2s"
		sv := cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)
		sv["quiet"], sv["repos"] = "1s", []string{repo}
	})
	wt := &wakeTest{r: r, conv: conv, repo: repo, clk: &wakeClock{}}
	wakerHook = func(w *supervisor.Waker) { wt.mu.Lock(); wt.waker = w; wt.mu.Unlock() }
	t.Cleanup(func() { wakerHook = nil })
	stop, owner, sv := wt.start(t)
	defer stop()
	w, pos := wt.worker(t, owner, "c1", "w", "reply one")
	wt.read(t, pos)
	wt.wokenN(t, owner, sv.ID, 1)
	wt.again(t, owner, w, "w-2", "reply two", 2)
	if d := wt.wokenN(t, owner, sv.ID, 2)[1]; !strings.Contains(d, "Output since the last digest:\n> two") {
		t.Fatalf("the second digest:\n%s", d)
	}
	// Asleep, the supervisor's agent loses its conversation, as its own
	// retention may remove it.
	eventually(t, "the supervisor asleep", 20*time.Second, func() bool {
		var state string
		wt.db.QueryRow(`SELECT state FROM threads WHERE id = ?`, int64(sv.ID)).Scan(&state)
		return state == string(control.Asleep)
	})
	var agentSession string
	wt.db.QueryRow(`SELECT agent_session_id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, int64(sv.ID)).Scan(&agentSession)
	if err := os.Remove(filepath.Join(wt.conv, filepath.Base(agentSession))); err != nil {
		t.Fatal(err)
	}
	wt.again(t, owner, w, "w-3", "reply three", 3)
	wt.wokenN(t, owner, sv.ID, 3)
	eventually(t, "the supervisor's session replaced", 15*time.Second, func() bool {
		var n int
		wt.db.QueryRow(`SELECT count(*) FROM events WHERE thread_id = ? AND kind = 'session_replaced'`, int64(sv.ID)).Scan(&n)
		return n == 1
	})
	wt.again(t, owner, w, "w-4", "reply four", 4)
	d := wt.wokenN(t, owner, sv.ID, 4)[3]
	if !strings.Contains(d, "Last output:") || !strings.Contains(d, "> two") || !strings.Contains(d, "> four") || strings.Contains(d, "since the last digest") {
		t.Fatalf("the first digest made for the new session:\n%s", d)
	}
}

// A wake stored for a supervisor's thread that a person then closed goes
// to the thread that replaces it, as it was made: it was made for the old
// session, so the next digest gives the new session the last output.
func TestDigestAfterAStoredWakeGoesToANewThread(t *testing.T) {
	wt := newWakeTest(t, budget(0, 0, 0))
	stop, owner, first := wt.start(t)
	w, pos := wt.worker(t, owner, "c1", "w", "reply one")
	wt.read(t, pos)
	wt.wokenN(t, owner, first.ID, 1)
	// The next wake is stored, and the daemon stops before it is sent.
	stopped := errors.New("stopped here")
	h := func(at string) error {
		if at == "stored" {
			return stopped
		}
		return nil
	}
	wt.hook.Store(&h)
	stop()
	stop, owner, _ = wt.start(t)
	wt.again(t, owner, w, "w-2", "reply two", 2)
	eventually(t, "the wake stored", 20*time.Second, func() bool {
		wt.clock().advance(30 * time.Second)
		var sending sql.NullInt64
		wt.db.QueryRow(`SELECT send_to FROM supervisor_wakes WHERE scope = 'c1'`).Scan(&sending)
		return sending.Valid
	})
	if err := owner.CloseThread(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	stop()
	stop, owner, sv := wt.start(t)
	defer stop()
	if sv.ID == first.ID {
		t.Fatalf("the closed supervisor %d was not replaced", first.ID)
	}
	if d := wt.wokenN(t, owner, sv.ID, 1)[0]; !strings.Contains(d, "Output since the last digest:\n> two") {
		t.Fatalf("the stored wake, sent to the new thread:\n%s", d)
	}
	wt.again(t, owner, w, "w-3", "reply three", 3)
	d := wt.wokenN(t, owner, sv.ID, 2)[1]
	if !strings.Contains(d, "Last output:") || !strings.Contains(d, "> one") || !strings.Contains(d, "> three") {
		t.Fatalf("the first digest made for the new thread:\n%s", d)
	}
}
