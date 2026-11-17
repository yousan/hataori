package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The end-to-end suite for a turn from start to end (design, section 8),
// with the daemon, a session host, the fake agent and the fake surface.

// order posts a trigger into c1 and returns the thread it opens: the n-th
// in this root.
func (r *chatRoot) order(t *testing.T, n int, line string) surface.Conversation {
	t.Helper()
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), line)
	return r.thread(t, n)
}

// count returns how many posts in conv have text.
func (r *chatRoot) count(conv surface.Conversation, text string) int {
	n := 0
	for _, c := range r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && c.Message.Text == text {
			n++
		}
	}
	return n
}

// db opens the instance's database to read it beside the daemon.
func (r *chatRoot) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.root, store.File)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// clock is a clock for the daemon's front end that a test moves forward.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.offset) }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// withClock gives the front ends of the daemons this test starts c as
// their clock, checked often.
func withClock(t *testing.T, c *clock) {
	frontEndHook = func(fe *chat.FrontEnd) { fe.Now, fe.Tick, fe.Outbox.Now = c.now, 10*time.Millisecond, c.now }
	t.Cleanup(func() { frontEndHook = nil })
}

func TestFollowUpContinuesTheSameAgentSession(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply first")
	r.posted(t, th, "first")
	db := r.db(t)
	var before string
	db.QueryRow(`SELECT agent_session_id FROM sessions`).Scan(&before)
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply second")
	r.posted(t, th, "second")
	var n int
	var after string
	db.QueryRow(`SELECT count(*), max(agent_session_id) FROM sessions`).Scan(&n, &after)
	if n != 1 || before == "" || after != before {
		t.Fatalf("%d sessions, agent session %q then %q; want one, the same", n, before, after)
	}
}

func TestLongTurnEndsWithItsAnswerPostedOnce(t *testing.T) {
	c := &clock{}
	withClock(t, c)
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" tool wait 8s\nreply done after a long time")
	r.posted(t, th, "tool wait 8s\nreply done after a long time")
	eventually(t, "the tool to run", 5*time.Second, func() bool { return len(r.events(t, "tool_started", th)) == 1 })
	for i := 0; i < 30; i++ { // half an hour on the front end's clock, while the tool runs
		c.add(time.Minute)
		time.Sleep(15 * time.Millisecond)
	}
	if len(r.events(t, "tool_ended", th)) != 0 {
		t.Fatal("the tool ended before the half hour passed on the front end's clock; the turn was not long")
	}
	r.posted(t, th, "done after a long time")
	eventually(t, "the end event", 10*time.Second, func() bool { return len(r.events(t, "turn_ended", th)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := r.count(th, "done after a long time"); n != 1 {
		t.Fatalf("the answer was posted %d times", n)
	}
	if ended := r.events(t, "turn_ended", th); len(ended) != 1 || ended[0]["outcome"] != "completion" {
		t.Fatalf("turn_ended %v", ended)
	}
}

func TestTurnTheAgentStartsHasOneEndEvent(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" background 300ms\nreply started a task")
	r.posted(t, th, "background done")
	var ended []map[string]any
	eventually(t, "two end events", 10*time.Second, func() bool {
		ended = r.events(t, "turn_ended", th)
		return len(ended) >= 2
	})
	time.Sleep(200 * time.Millisecond)
	ended = r.events(t, "turn_ended", th)
	if len(ended) != 2 || ended[0]["by"] != "caller" || ended[0]["background"] != 1.0 ||
		ended[1]["by"] != "agent" || ended[1]["background"] != 0.0 {
		t.Fatalf("turn_ended %v; want the caller's with 1 task running, then one the agent started with none", ended)
	}
}

func TestQuestionIsShownBlocksAndTakesTheNextPost(t *testing.T) {
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" ask Which colour?")
	r.posted(t, th, "Which colour?")
	eventually(t, "a blocked end", 10*time.Second, func() bool {
		e := r.events(t, "turn_ended", th)
		return len(e) == 1 && e[0]["outcome"] == "blocked"
	})
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "blue")
	r.posted(t, th, "answer: blue")
}

func TestLimitLoginAndCrashEachGiveOneLineAndOneFailure(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) { cfg["login_relay"] = false })
	startDaemon(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	for i, tc := range []struct{ script, reason, line string }{
		{"fail limit", "limit", "The turn failed: the usage limit was reached."},
		{"fail login", "login", "The turn failed: the agent is not logged in."},
		{"exit", "crashed", "The turn failed: the agent crashed."},
	} {
		th := r.order(t, i, "!go repo:"+repo+" "+tc.script)
		r.posted(t, th, tc.line)
		eventually(t, "the failure of "+tc.script, 10*time.Second, func() bool { return len(r.events(t, "turn_ended", th)) > 0 })
		time.Sleep(100 * time.Millisecond)
		ended := r.events(t, "turn_ended", th)
		if len(ended) != 1 || ended[0]["outcome"] != "failure" || ended[0]["reason"] != tc.reason || r.count(th, tc.line) != 1 {
			t.Fatalf("%s: turn_ended %v, %d lines", tc.script, ended, r.count(th, tc.line))
		}
	}
}

func TestProgressLineAppearsIsEditedAndRemoved(t *testing.T) {
	c := &clock{}
	withClock(t, c)
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" tool wait 8s\nreply finished")
	r.posted(t, th, "tool wait 8s\nreply finished")
	eventually(t, "the tool to run", 10*time.Second, func() bool { return len(r.events(t, "tool_started", th)) == 1 })
	line := func() (posted, edited, deleted int) {
		var ref fakesurface.MessageRef
		for _, call := range r.driver.Calls(th) {
			switch {
			case call.Op == fakesurface.OpPost && strings.HasPrefix(call.Message.Text, "Still working"):
				posted++
				ref = call.Ref
			case call.Op == fakesurface.OpEdit && call.Ref == ref:
				edited++
			case call.Op == fakesurface.OpDelete && call.Ref == ref:
				deleted++
			}
		}
		return
	}
	c.add(95 * time.Second)
	eventually(t, "the line", 5*time.Second, func() bool { p, _, _ := line(); return p == 1 })
	c.add(20 * time.Second)
	eventually(t, "an edit", 5*time.Second, func() bool { _, e, _ := line(); return e >= 1 })
	if len(r.events(t, "tool_ended", th)) != 0 {
		t.Fatal("the tool ended before the line was edited")
	}
	r.posted(t, th, "finished")
	eventually(t, "the line removed", 5*time.Second, func() bool { _, _, d := line(); return d == 1 })
	if p, _, _ := line(); p != 1 {
		t.Fatalf("%d lines posted", p)
	}
}

func TestModelAndEffortReachTheAgentAndTheNextThreadGetsTheDefaults(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["model"] = "channel-model"
		cfg["scopes"].([]map[string]any)[0]["effort"] = "medium"
	})
	startDaemon(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	th := r.order(t, 0, "!go model:given-model effort:high repo:"+repo+" whoami")
	r.posted(t, th, "model given-model effort high")
	th = r.order(t, 1, "!go repo:"+repo+" whoami")
	r.posted(t, th, "model channel-model effort medium")
}

func TestLoginOutageWithTheFakes(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "agent-config")
	os.MkdirAll(configDir, 0o700)
	const code = "CODE-7f3a"
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, ConfigDir: configDir, Login: fake.Login{Code: code}}}
	})
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	repo := bareRepo(t)
	a := r.order(t, 0, "!go repo:"+repo+" reply one")
	r.posted(t, a, "The turn failed: the agent is not logged in.")
	b := r.order(t, 1, "!go repo:"+repo+" reply two")
	r.posted(t, b, "The turn failed: the agent is not logged in.")
	dm, _ := r.driver.OpenDirect(nil, e2eOwner)
	var link string
	defer func() {
		if t.Failed() {
			for _, c := range r.driver.Calls(dm) {
				t.Logf("dm %v, %d bytes", c.Op, len(c.Message.Text)) // not the text: it holds the link
			}
		}
	}()
	eventually(t, "the link in the owner's message", 15*time.Second, func() bool {
		for _, c := range r.driver.Calls(dm) {
			if i := strings.Index(c.Message.Text, "https://login.example.invalid/"); i >= 0 && strings.Contains(c.Message.Text, "Stopped threads (2)") {
				link = strings.Fields(c.Message.Text[i:])[0]
				return true
			}
		}
		return false
	})
	r.driver.InjectDirect(e2eOwner, code)
	for _, th := range []surface.Conversation{a, b} {
		r.posted(t, th, "The agent is logged in again. Send your last message again to go on.")
	}
	r.posted(t, dm, "Logged in again.")
	posts := 0
	for _, c := range r.driver.Calls(dm) {
		if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "is logged out") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("%d direct messages about the outage", posts)
	}
	// The link and the code: in no log, table or event.
	// The agent is logged in: a stopped thread runs a turn again.
	if _, err := os.Stat(filepath.Join(configDir, fake.LoggedInFile)); err != nil {
		t.Fatalf("the fake agent is not logged in: %v", err)
	}
	r.driver.Inject(a, fakesurface.Webhook(e2eHook), "reply one again")
	r.posted(t, a, "one again")
	for i, secret := range []string{link, code} {
		name := []string{"the link", "the code"}[i]
		if strings.Contains(logged(), secret) {
			t.Fatalf("%s is in the daemon's log", name)
		}
		// Every file under the root (host logs, event logs, transcripts, the
		// database), but the config: the fake's section names the code it
		// accepts.
		filepath.WalkDir(r.root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && p == filepath.Join(r.root, "config") {
				return filepath.SkipDir
			}
			if err != nil || d.IsDir() || strings.Contains(p, string(filepath.Separator)+".git"+string(filepath.Separator)) {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), secret) {
				t.Errorf("%s is in %s", name, p)
			}
			return nil
		})
	}
	db := r.db(t)
	rows, _ := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, table := range tables {
		cols, _ := db.Query(`SELECT * FROM ` + table)
		names, _ := cols.Columns()
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for cols.Next() {
			cols.Scan(ptrs...)
			for _, v := range vals {
				s := ""
				switch v := v.(type) {
				case string:
					s = v
				case []byte:
					s = string(v)
				}
				if strings.Contains(s, link) || strings.Contains(s, code) {
					cols.Close()
					t.Fatalf("a secret is in table %s", table)
				}
			}
		}
		cols.Close()
	}
}
