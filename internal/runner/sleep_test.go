package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
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

func count(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n)
	return n
}

// A running turn, an open question and a background task each keep the
// session awake.
func TestBusySessionStaysAwake(t *testing.T) {
	for name, script := range map[string]string{
		"running":    "tool wait 5s",
		"waiting":    "ask which?",
		"background": "background 5s\nreply started",
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

// As after a host reboot: every agent and host is gone, nothing recorded
// their end. Each thread gets one notice, and its next input resumes it.
func TestRebootThenResume(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	threads := []control.ThreadID{{Scope: "s", Key: "t1"}, {Scope: "s", Key: "t2"}}
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
	rows, _ := st.DB.Query(`SELECT thread, count(*) FROM events WHERE kind = 'state'
		AND json_extract(data, '$.to') = 'asleep' AND json_extract(data, '$.reason') = 'interrupted' GROUP BY thread`)
	notices := map[string]int{}
	for rows.Next() {
		var th string
		var n int
		rows.Scan(&th, &n)
		notices[th] = n
	}
	rows.Close()
	if notices["t1"] != 1 || notices["t2"] != 1 {
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
