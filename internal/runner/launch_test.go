package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// hosts returns the pids of the session host processes of stateDir. It
// asks ps, as macOS has no /proc.
func hosts(t *testing.T, stateDir string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		t.Fatal("ps:", err)
	}
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		pid, args, _ := strings.Cut(strings.TrimSpace(line), " ")
		if strings.HasSuffix(args, " role host "+stateDir) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// A daemon killed while it launches a new session's host leaves, after
// it starts again, exactly one host, and the first input is delivered once.
func TestDaemonKilledDuringALaunch(t *testing.T) {
	for _, point := range []string{"before host start", "after host start"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			t.Cleanup(func() { killSessions(root) })
			runDaemon(t, root, false, "die-at", point, "reply once").waitKilled(t)
			st := openStore(t, root)
			d := runDaemon(t, root, false, "resume")
			until(t, "the turn's end", st, "turn_ended", 1)
			time.Sleep(300 * time.Millisecond) // anything late would show now
			d.kill()
			got := counts(t, st)
			if got["turn_started"] != 1 || got["input"] != 1 || got["text"] != 1 {
				t.Errorf("events %v", got)
			}
			if h := hosts(t, StateDir(root, 1)); len(h) != 1 {
				t.Errorf("%d hosts run for the session: %v", len(h), h)
			}
			var launching int
			st.DB.QueryRow(`SELECT launch FROM sessions WHERE id = 1`).Scan(&launching)
			if launching != 0 {
				t.Errorf("launch = %d after the restart", launching)
			}
		})
	}
}

// A daemon killed after a new thread's workspace is in place and before
// it recorded it: after a restart the session starts in that workspace,
// whose contents are as they were, and the first input is delivered once.
func TestDaemonKilledAfterTheWorkspaceIsPlaced(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	runDaemon(t, root, false, "die-at+dieplaced", "no such point", "reply once").waitKilled(t)
	work := filepath.Join(root, "work", "1")
	before, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("the workspace is not in place: %v", err)
	}
	st := openStore(t, root)
	d := runDaemon(t, root, false, "resume")
	until(t, "the turn's end", st, "turn_ended", 1)
	time.Sleep(300 * time.Millisecond)
	d.kill()
	got := counts(t, st)
	if got["turn_started"] != 1 || got["input"] != 1 || got["text"] != 1 {
		t.Errorf("events %v", got)
	}
	var dir string
	st.DB.QueryRow(`SELECT json_extract(spec, '$.spec.Dir') FROM sessions WHERE id = 1`).Scan(&dir)
	if dir != work {
		t.Errorf("the session runs in %q, want %q", dir, work)
	}
	after, _ := os.ReadDir(work)
	notes, _ := os.ReadFile(filepath.Join(work, "notes.txt"))
	if len(after) != len(before) || len(after) != 1 || string(notes) != "someone's work\n" {
		t.Errorf("the workspace changed: %v, then %v; notes %q", before, after, notes)
	}
}

// A daemon killed after it stored that a session could not resume, and
// before it launched the session that replaces it: after a restart the new
// session starts, and its agent gets the input the old one was woken for,
// once, with its link, its key and the text a plugin added. Killed right
// after the old session's failure is committed (#527) or after the new
// session is inserted (#508).
func TestDaemonKilledAfterAReplace(t *testing.T) {
	for _, c := range []struct {
		point    string
		terminal bool
	}{{"after cannot resume", false}, {"after cannot resume", true}, {"after replace", false}, {"after replace", true}} {
		terminal := c.terminal
		t.Run(fmt.Sprintf("%s terminal %v", c.point, terminal), func(t *testing.T) {
			root := t.TempDir()
			t.Cleanup(func() { killSessions(root) })
			t.Cleanup(func() {
				if t.Failed() {
					b, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
					t.Logf("daemon log:\n%s", b)
				}
			})
			runDaemon(t, root, terminal, "die-replace+idle+forget", c.point, "reply hello", "reply anew").waitKilled(t)
			st := openStore(t, root)
			var sessions, launch int
			var old, now core.FirstInput
			var fo, fn string
			st.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
			st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = 1`).Scan(&fo)
			st.DB.QueryRow(`SELECT launch, coalesce(first_input, '') FROM sessions WHERE id = 2`).Scan(&launch, &fn)
			json.Unmarshal([]byte(fo), &old)
			json.Unmarshal([]byte(fn), &now)
			if sessions != 2 || launch != 1 {
				t.Fatalf("after the kill: %d sessions, the new one's launch %d", sessions, launch)
			}
			if now.Key != "m2" || now.Text != "reply anew" || now.Before != "plugin before" || now.After != "plugin after" ||
				now.Link == nil || *now.Link != (control.LinkRef{Client: "chat", Channel: "s", Thread: "t1"}) ||
				now.Key != old.Key || now.Text != old.Text || now.Wake || now.Sent || now.Written || now.Dir == "" ||
				!strings.Contains(now.Context, "reply hello") {
				t.Fatalf("the new session's first input %s, the old one's %s", fn, fo)
			}
			d := runDaemon(t, root, terminal, "resume")
			until(t, "the new session's turn", st, "turn_ended", 2)
			time.Sleep(300 * time.Millisecond) // anything late would show now
			d.kill()
			var anew, inSession2 int
			st.DB.QueryRow(`SELECT count(*), coalesce(sum(json_extract(data, '$.session') = 2), 0) FROM events
				WHERE kind = 'text' AND json_extract(data, '$.text') = 'anew'`).Scan(&anew, &inSession2)
			if anew != 1 || inSession2 != 1 {
				t.Errorf("the agent answered the input %d times, %d of them in the new session", anew, inSession2)
			}
			if got := counts(t, st); got["turn_started"] != 2 || got["input"] != 2 || got["session_replaced"] != 1 {
				t.Errorf("events %v", got)
			}
			// The agent got the plugins' text with the input, as the input
			// event of the new session records it.
			var before, after, link string
			st.DB.QueryRow(`SELECT json_extract(data, '$.before'), json_extract(data, '$.after'), json_extract(data, '$.link.thread') FROM events
				WHERE kind = 'input' AND json_extract(data, '$.session') = 2`).Scan(&before, &after, &link)
			if before != "plugin before" || after != "plugin after" || link != "t1" {
				t.Errorf("the new session's input came with %q and %q, through %q", before, after, link)
			}
		})
	}
}

// A daemon killed after it stored that a session woken by an attach could
// not resume: after a restart the session that replaces it is started with
// no input, and no turn starts for it (#528).
func TestDaemonKilledReplacingAnAttachedSession(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { killSessions(root) })
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
			t.Logf("daemon log:\n%s", b)
		}
	})
	runDaemon(t, root, true, "die-replace+idle+forget", "after cannot resume", "reply hello").waitKilled(t)
	st := openStore(t, root)
	var fi string
	st.DB.QueryRow(`SELECT coalesce(first_input, '') FROM sessions WHERE id = 2`).Scan(&fi)
	var now core.FirstInput
	if json.Unmarshal([]byte(fi), &now); !now.NoInput || now.Wake || now.Text != "" {
		t.Fatalf("the new session's first input %q", fi)
	}
	d := runDaemon(t, root, true, "resume")
	waitFor(t, "the new session's agent", 15*time.Second, func() bool {
		var id string
		st.DB.QueryRow(`SELECT coalesce(agent_session_id, '') FROM sessions WHERE id = 2`).Scan(&id)
		return id != ""
	})
	time.Sleep(time.Second) // a turn for it would start now
	d.kill()
	var state string
	st.DB.QueryRow(`SELECT state FROM sessions WHERE id = 2`).Scan(&state)
	if state != "idle" && state != "asleep" {
		t.Errorf("the new session is %s", state)
	}
	if got := counts(t, st); got["turn_started"] != 1 || got["input"] != 1 || got["session_replaced"] != 1 {
		t.Errorf("events %v", got)
	}
}

// A session is started once in a process: a second Start of it, as from
// Recover while a replace started it, starts no second host and changes
// nothing (#508).
func TestStartTwiceStartsOnce(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	until(t, "the turn's end", f.st, "turn_ended", 1)
	var spec string
	f.st.DB.QueryRow(`SELECT spec FROM sessions WHERE id = 1`).Scan(&spec)
	if err := f.run.Start(context.Background(), core.Launch{Session: 1, Thread: 1, Agent: "fake", Dir: f.root,
		Input: control.Input{Text: "reply again"}}); err != nil {
		t.Fatalf("a second Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := counts(t, f.st); got["turn_started"] != 1 || got["text"] != 1 {
		t.Errorf("events %v", got)
	}
	if h := hosts(t, StateDir(f.root, 1)); len(h) != 1 {
		t.Errorf("%d hosts run for the session: %v", len(h), h)
	}
	var again string
	f.st.DB.QueryRow(`SELECT spec FROM sessions WHERE id = 1`).Scan(&again)
	if again != spec {
		t.Errorf("the session's spec changed: %s, then %s", spec, again)
	}
}
