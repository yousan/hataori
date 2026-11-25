package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/proctest"
	"github.com/yousan/hataori/internal/store"
)

func sessionState(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var s string
	st.DB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&s)
	return s
}

// agentAlive reports whether the agent the host of session id started is
// still running.
func agentAlive(t *testing.T, root string, id int64) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(StateDir(root, id), host.PIDFile))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid > 1 && syscall.Kill(pid, 0) == nil
}

func send(t *testing.T, c control.API, th control.ThreadID, key, text string) {
	t.Helper()
	if err := c.Send(context.Background(), th, key, control.Input{Text: text}, control.Refuse); err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
}

// readyLines returns the fake's start-up reports in the session's log.
func readyLines(t *testing.T, root string, id int64) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(StateDir(root, id), host.EventsFile))
	var out []map[string]any
	for _, l := range bytes.Split(b, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(l, &m) != nil {
			continue
		}
		if p, ok := m["payload"]; ok { // a hook call, in the terminal form
			m, _ = p.(map[string]any)
		}
		if m["t"] == "ready" {
			out = append(out, m)
		}
	}
	return out
}

func TestIdleSessionSleepsAndResumes(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		f := newFixture(t, options{terminal: terminal, idlePeriod: 300 * time.Millisecond})
		f.start("reply hello")
		waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
		waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
		var id string
		f.st.DB.QueryRow(`SELECT agent_session_id FROM sessions WHERE id = 1`).Scan(&id)

		send(t, owner(f.core), thread, "m2", "reply again")
		waitFor(t, "the resumed turn", 15*time.Second, f.has("turn_ended", 2))
		texts := f.eventData("text")
		if last := texts[len(texts)-1]; last["text"] != "again" {
			t.Fatalf("texts %v", texts)
		}
		ready := readyLines(t, f.root, 1)
		if len(ready) != 2 || ready[1]["resume"] != id || ready[1]["session"] != id {
			t.Fatalf("start-up reports %v; want the second to resume %s", ready, id)
		}
		if n := count(t, f.st, "sessions"); n != 1 {
			t.Fatalf("%d sessions", n)
		}
	})
}

// Sleep puts an idle session to sleep at once, but not one whose agent was
// given input that no turn has reported yet, such as a message its codec
// held; input an earlier run left, which nothing reports any more, does not
// keep it awake, though a row of its id is made again.
func TestSleepNow(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	waitFor(t, "the turn's end", 15*time.Second, f.has("turn_ended", 1))
	sleep := func(want bool) {
		t.Helper()
		if slept, err := f.run.Sleep(context.Background(), thread); err != nil || slept != want {
			t.Fatalf("Sleep: %v, %v; want %v", slept, err, want)
		}
	}
	left := func() {
		t.Helper()
		if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs (id, session_id, request, data, written) VALUES (10, 1, '', '{}', 1)`); err != nil {
			t.Fatal(err)
		}
	}
	left()
	sleep(false)
	if s := sessionState(t, f.st, 1); s != "idle" {
		t.Fatalf("the session is %s, want idle", s)
	}
	f.st.DB.Exec(`DELETE FROM pending_inputs`)
	sleep(true)
	if s := sessionState(t, f.st, 1); s != "asleep" {
		t.Fatalf("the session is %s, want asleep", s)
	}
	sleep(false) // asleep already

	left() // by the run before
	send(t, owner(f.core), thread, "m2", "reply again")
	waitFor(t, "the resumed turn", 15*time.Second, f.has("turn_ended", 2))
	sleep(true) // what the run before left does not keep it awake
	send(t, owner(f.core), thread, "m3", "reply once more")
	waitFor(t, "the next resumed turn", 15*time.Second, f.has("turn_ended", 3))
	f.st.DB.Exec(`DELETE FROM pending_inputs`)
	left() // this run's, with the id of the one before
	sleep(false)
}

func count(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n)
	return n
}

// A running turn, an open question and a background task each keep the
// session awake, the last one also after a turn that failed.
func TestBusySessionStaysAwake(t *testing.T) {
	for name, script := range map[string]string{
		"running":    "tool wait 5s",
		"waiting":    "ask which?",
		"background": "background 5s\nreply started",
		"failed":     "background 5s\nfail api_error",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, options{idlePeriod: 200 * time.Millisecond})
			f.start(script)
			waitFor(t, "the turn", 15*time.Second, f.has("turn_started", 1))
			time.Sleep(2500 * time.Millisecond) // two idle checks past the idle period
			if s := sessionState(t, f.st, 1); s == "asleep" || !agentAlive(t, f.root, 1) {
				t.Fatalf("state %s, agent alive %v", s, agentAlive(t, f.root, 1))
			}
		})
	}
}

// A turn that failed, or was stopped, and left background tasks whose end is
// never learned: they keep the session awake, against the idle period and a
// Sleep, for the background wait after its last news. Tasks that a completed
// turn left do so without a bound.
func TestTasksAFailedTurnLeftKeepTheSessionAwakeForTheWait(t *testing.T) {
	const wait = 3 * time.Second
	for name, tc := range map[string]struct {
		script string
		asleep bool
	}{
		"failed":    {"background 60s\nfail api_error", true},
		"limit":     {"background 60s\nfail limit", true},
		"completed": {"background 60s\nreply started", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, options{idlePeriod: 200 * time.Millisecond, background: wait})
			f.start(tc.script)
			waitFor(t, "the turn's end", 15*time.Second, f.has("turn_ended", 1))
			var ended int64
			f.st.DB.QueryRow(`SELECT ended_at FROM turns`).Scan(&ended)
			if slept, err := f.run.Sleep(context.Background(), thread); err != nil || slept {
				t.Fatalf("Sleep within the wait: %v, %v", slept, err)
			}
			if !tc.asleep {
				time.Sleep(wait + 2500*time.Millisecond) // two idle checks past the wait
				if s := sessionState(t, f.st, 1); s != "idle" {
					t.Fatalf("after the wait the session is %s, want idle", s)
				}
				return
			}
			waitFor(t, "the sleep", wait+15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
			var slept int64
			f.st.DB.QueryRow(`SELECT updated_at FROM sessions`).Scan(&slept)
			if d := time.Duration(slept-ended) * time.Millisecond; d < wait {
				t.Fatalf("asleep %v after the turn's end, want at least %v", d, wait)
			}
		})
	}
}

// As after a host reboot: every agent and host is gone, nothing recorded
// their end. Each thread gets one notice, and its next input resumes it.
func TestRebootThenResume(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	threads := []control.ThreadID{1, 2}
	d := runDaemon(t, root, false, "start", "tool wait 60s", "tool wait 60s")
	st0 := openStore(t, root)
	until(t, "both tools", st0, "tool_started", 2)
	d.kill()           // the daemon goes
	killSessions(root) // and every process with it

	log := &syncBuffer{}
	st, c, r, err := build(root, options{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st.Close() }()
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both asleep", 20*time.Second, func() bool {
		return sessionState(t, st, 1) == "asleep" && sessionState(t, st, 2) == "asleep"
	})
	rows, _ := st.DB.Query(`SELECT thread_id, count(*) FROM events WHERE kind = 'state'
		AND json_extract(data, '$.to') = 'asleep' AND json_extract(data, '$.reason') = 'interrupted' GROUP BY thread_id`)
	notices := map[int64]int{}
	for rows.Next() {
		var th int64
		var n int
		rows.Scan(&th, &n)
		notices[th] = n
	}
	rows.Close()
	if notices[1] != 1 || notices[2] != 1 {
		t.Fatalf("notices per thread: %v", notices)
	}

	send(t, owner(c), threads[0], "m2", "reply back")
	waitFor(t, "the resumed turn", 15*time.Second, func() bool {
		var n int
		st.DB.QueryRow(`SELECT count(*) FROM events WHERE kind = 'text' AND json_extract(data, '$.text') = 'back'`).Scan(&n)
		return n == 1
	})
	if s := sessionState(t, st, 1); s != "idle" {
		t.Fatalf("resumed session is %s\n%s", s, log.String())
	}
}

// The agent no longer has the conversation: the thread is told, and a new
// session starts in the same workspace with the input.
func TestCannotResumeStartsANewSession(t *testing.T) {
	forms(t, func(t *testing.T, terminal bool) {
		conv := t.TempDir()
		f := newFixture(t, options{terminal: terminal, idlePeriod: 300 * time.Millisecond, conversations: conv})
		f.start("reply hello")
		waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
		waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
		files, _ := filepath.Glob(filepath.Join(conv, "*"))
		for _, p := range files {
			os.Remove(p) // the agent's own retention removed it
		}

		send(t, owner(f.core), thread, "m2", "reply anew")
		waitFor(t, "the new session's turn", 15*time.Second, f.has("turn_ended", 2))
		if got := f.eventData("session_replaced"); len(got) != 1 || got[0]["reason"] != "cannot resume" {
			t.Fatalf("session_replaced events: %v", got)
		}
		texts := f.eventData("text")
		if last := texts[len(texts)-1]; last["text"] != "anew" || last["session"] != 2.0 {
			t.Fatalf("texts %v", texts)
		}
		var d1, d2, id1, id2 string
		f.st.DB.QueryRow(`SELECT json_extract(spec, '$.spec.Dir'), agent_session_id FROM sessions WHERE id = 1`).Scan(&d1, &id1)
		f.st.DB.QueryRow(`SELECT json_extract(spec, '$.spec.Dir'), agent_session_id FROM sessions WHERE id = 2`).Scan(&d2, &id2)
		if d1 == "" || d1 != d2 || id1 == id2 {
			t.Fatalf("workspaces %q, %q; session ids %q, %q", d1, d2, id1, id2)
		}
		if s := sessionState(t, f.st, 1); s != "failed" {
			t.Fatalf("old session is %s", s)
		}
	})
}

// A session woken with no input, as an attach wakes it, whose agent no
// longer has the conversation is replaced by a session whose agent is
// started with no input: no turn starts for it (#528).
func TestCannotResumeAfterAnAttachStartsNoTurn(t *testing.T) {
	conv := t.TempDir()
	f := newFixture(t, options{terminal: true, idlePeriod: 300 * time.Millisecond, conversations: conv})
	f.start("reply hello")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
	files, _ := filepath.Glob(filepath.Join(conv, "*"))
	for _, p := range files {
		os.Remove(p) // the agent's own retention removed it
	}

	// The attach wakes the session and waits for a terminal that never
	// comes, as its agent cannot resume; it gives up when ctx ends.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	attached := make(chan struct{})
	t.Cleanup(func() { cancel(); <-attached })
	go func() {
		defer close(attached)
		f.core.As(control.Caller{From: ownerFrom, Scopes: []string{"s"}, May: all | control.OpAttach}).AttachTerminal(ctx, thread)
	}()
	waitFor(t, "the new session", 15*time.Second, f.has("session_replaced", 1))
	waitFor(t, "the new session's agent", 15*time.Second, func() bool {
		var id string
		f.st.DB.QueryRow(`SELECT coalesce(agent_session_id, '') FROM sessions WHERE id = 2`).Scan(&id)
		return id != ""
	})
	time.Sleep(time.Second) // a turn for it would start now
	if s := sessionState(t, f.st, 2); s != "idle" && s != "asleep" {
		t.Fatalf("the new session is %s\n%s", s, f.log.String())
	}
	got := counts(t, f.st)
	if got["turn_started"] != 1 || got["input"] != 1 {
		t.Errorf("events %v", got)
	}
	var fi string
	f.st.DB.QueryRow(`SELECT coalesce(first_input, '') FROM sessions WHERE id = 2`).Scan(&fi)
	if fi != "" {
		t.Errorf("the new session's first input is still %s", fi)
	}
}

// A daemon restart by itself puts no session to sleep.
func TestRestartAloneSleepsNothing(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	o := options{idlePeriod: 4 * time.Second}
	st, c, r, err := build(root, o, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner(c).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
		Repo: "file:///unused", Input: control.Input{Text: "reply hi"}}); err != nil {
		t.Fatal(err)
	}
	until(t, "the turn's end", st, "turn_ended", 1)
	r.Close()
	st.Close()

	st, _, r, err = build(root, o, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st.Close() }()
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if s := sessionState(t, st, 1); s != "idle" || !agentAlive(t, root, 1) {
		t.Fatalf("state %s, agent alive %v", s, agentAlive(t, root, 1))
	}
}

// The daemon stopped after putting a session to sleep and before its agent
// was stopped: the next daemon stops it.
func TestAsleepWithARunningAgentIsStopped(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	d := runDaemon(t, root, false, "start", "reply hi")
	st := openStore(t, root)
	until(t, "the turn's end", st, "turn_ended", 1)
	d.kill()
	if !agentAlive(t, root, 1) {
		t.Fatal("the agent is not running")
	}
	st.DB.Exec(`UPDATE sessions SET state = 'asleep' WHERE id = 1`) // committed, then the daemon died

	st2, _, r, err := build(root, options{}, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st2.Close() }()
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, root, 1) })
}

// wokenWithItsHostRunning leaves session 1 as a daemon that died leaves a
// session it woke with "reply anew" while its old host had not recorded its
// end yet: the wake committed, nothing launched, and the old host running.
func wokenWithItsHostRunning(t *testing.T, root string) *store.Store {
	t.Helper()
	d := runDaemon(t, root, false, "start", "reply hi")
	st := openStore(t, root)
	until(t, "the turn's end", st, "turn_ended", 1)
	d.kill()
	if hostEnded(root, 1) {
		t.Fatal("the host has ended")
	}
	fi, _ := json.Marshal(firstInput{Principal: "owner", From: ownerFrom, Key: "m2", Text: "reply anew", Wake: true})
	if _, err := st.DB.Exec(`UPDATE sessions SET state = 'creating', first_input = ?, launch = 1 WHERE id = 1`, string(fi)); err != nil {
		t.Fatal(err)
	}
	return st
}

// A wake that a daemon started again recovers stops the agent that was put
// to sleep when it still runs, waits for its host to record its end, and
// the new agent gets the input (#549, #713).
func TestRecoveredWakeStopsTheOldAgent(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	st := wokenWithItsHostRunning(t, root)
	log := &syncBuffer{}
	st2, c, r, err := build(root, options{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st2.Close() }()
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	until(t, "the woken turn's end", st, "turn_ended", 2)
	var texts []string
	rows, _ := st.DB.Query(`SELECT json_extract(data, '$.text') FROM events WHERE kind = 'text' ORDER BY pos`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		texts = append(texts, s)
	}
	rows.Close()
	if len(texts) != 2 || texts[1] != "anew" {
		t.Fatalf("texts %q; session %s\n%s", texts, sessionState(t, st, 1), log.String())
	}
}

// A message to an asleep session whose agent the daemon started again is
// still stopping starts the next agent after that stop, however late it
// is: the stop does not reach the next agent (#713).
func TestLateStopOfASleeperSparesTheNextAgent(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	st := wokenWithItsHostRunning(t, root)
	if _, err := st.DB.Exec(`UPDATE sessions SET state = 'asleep', first_input = '', launch = 0 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	st2, c, r, err := build(root, options{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st2.Close() }()
	var seen atomic.Bool
	late := make(chan struct{})
	r.point = func(p string) {
		if p == "stop-earlier" && !seen.Swap(true) {
			<-late
		}
	}
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		sent <- owner(c).Send(context.Background(), 1, "m2", control.Input{Text: "tool hold 2s"}, control.Refuse)
	}()
	time.Sleep(time.Second) // a next agent that does not wait for the stop runs its turn by now
	close(late)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	until(t, "the woken turn's end", st, "turn_ended", 2)
	var end string
	st.DB.QueryRow(`SELECT data FROM events WHERE kind = 'turn_ended' ORDER BY pos DESC LIMIT 1`).Scan(&end)
	var started, ended int64
	st.DB.QueryRow(`SELECT max(time) FROM events WHERE kind = 'turn_started'`).Scan(&started)
	st.DB.QueryRow(`SELECT max(time) FROM events WHERE kind = 'turn_ended'`).Scan(&ended)
	if d := ended - started; d < 1900 {
		t.Fatalf("the next turn ended after %d ms, want 2 s: %s\n%s", d, end, log.String())
	}
}

// A wake recovered after a restart whose old host does not end within the
// bound, even when told to, fails, and says so (#549).
func TestRecoveredWakeFailsWhenTheOldHostRunsOn(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	st := wokenWithItsHostRunning(t, root)
	st2, c, r, err := build(root, options{}, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st2.Close() }()
	r.endWait = 500 * time.Millisecond
	// Its host gets no signal and answers nobody, until it is killed.
	if pid, ok := proctest.PID(filepath.Join(StateDir(root, 1), host.HostPIDFile)); !ok || syscall.Kill(pid, syscall.SIGSTOP) != nil {
		t.Fatal("the host was not stopped")
	}
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	st.DB.QueryRow(`SELECT json_extract(data, '$.reason') FROM events WHERE kind = 'state' AND json_extract(data, '$.to') = 'failed'`).Scan(&reason)
	if s := sessionState(t, st, 1); s != "failed" || !strings.Contains(reason, "its host is still running after 500ms") {
		t.Fatalf("session %s, reason %q", s, reason)
	}
}

// A session that failed while its agent still ran, as when the daemon died
// right after it stored a failure at a start-up dialog and before it
// stopped the host, has its agent stopped when a daemon reconnects (#423).
func TestReconnectStopsAFailedSessionsAgent(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	st, c, r, err := build(root, options{}, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner(c).Start(context.Background(), control.StartRequest{Key: "k1", Scope: "s", Thread: "t1",
		Repo: "file:///unused", Input: control.Input{Text: "reply hi"}}); err != nil {
		t.Fatal(err)
	}
	until(t, "the turn's end", st, "turn_ended", 1)
	r.Close()
	if _, err := st.DB.Exec(`UPDATE sessions SET state = 'failed' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if !agentAlive(t, root, 1) {
		t.Fatal("the agent ended with the runner")
	}

	st, _, r, err = build(root, options{}, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close(); st.Close() }()
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed session's agent to end", 15*time.Second, func() bool { return !agentAlive(t, root, 1) })
}
