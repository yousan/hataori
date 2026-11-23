package main

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// svRoot is a chat root with two scopes: c1 names a supervisor, the person
// sv1, and c2 names none. The fake agent takes confined specs when
// confines is set, and names fake-cheap its cheapest model. Each session
// the fake program starts leaves a file in the returned directory. Each of
// more changes the config after that.
func svRoot(t *testing.T, confines bool, more ...func(cfg map[string]any)) (*chatRoot, string) {
	t.Helper()
	conversations := t.TempDir()
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["owner"] = "boss"
		cfg["people"] = []map[string]any{
			{"name": "boss", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
			{"name": "sv1", "ids": []string{supervisor.ID("c1")}},
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch", "sv1"},
				"supervisor": map[string]any{"instructions": "Route the channel's work."}},
			{"key": "c2", "client": fakeSurface, "enabled": true, "people": []string{"dispatch"}},
		}
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Confines: confines, Cheap: "fake-cheap", Conversations: conversations}}
		for _, change := range more {
			change(cfg)
		}
	})
	return r, conversations
}

// supervisorThread waits until scope has its supervisor's thread in state,
// and returns it.
func supervisorThread(t *testing.T, c *wire.Client, scope string, state control.State) control.ThreadInfo {
	t.Helper()
	var got control.ThreadInfo
	eventually(t, "the supervisor's thread "+string(state), 15*time.Second, func() bool {
		ts, err := c.Threads(context.Background(), control.Filter{Scope: scope})
		if err != nil {
			return false
		}
		for _, th := range ts {
			if th.Title == "Supervisor of this channel" && th.State == state && (state != control.Idle || th.Turns > 0) {
				got = th
				return true
			}
		}
		return false
	})
	return got
}

// supervisorThreads counts the supervisor threads of scope.
func supervisorThreads(t *testing.T, c *wire.Client, scope string) int {
	t.Helper()
	ts, err := c.Threads(context.Background(), control.Filter{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, th := range ts {
		if th.Title == "Supervisor of this channel" {
			n++
		}
	}
	return n
}

// openedIn counts the chat threads opened in channel.
func (r *chatRoot) openedIn(channel string) int {
	n := 0
	for _, c := range r.driver.Calls(surface.Conversation{Channel: channel}) {
		if c.Op == fakesurface.OpOpenThread {
			n++
		}
	}
	return n
}

// dbOf opens root's database for reading.
func dbOf(t *testing.T, root string) *sql.DB {
	t.Helper()
	return (&chatRoot{root: root}).db(t)
}

// A scope that names a supervisor gets one thread for it, in its own
// channel, started by the daemon with the adapter's cheapest model; after
// two restarts it is found again, not started again. A scope that names
// none gets none.
func TestSupervisorThreadOnceAcrossRestarts(t *testing.T) {
	r, _ := svRoot(t, true)
	stop, logged := runDaemonLog(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	th := supervisorThread(t, owner, "c1", control.Idle)
	if th.Model != "fake-cheap" || th.StartedBy != "boss" || th.Repo != "" || len(th.Links) != 1 || th.Links[0].Channel != "c1" {
		t.Fatalf("the supervisor's thread %+v; want the cheapest model, the owner's, no repository, a thread in c1", th)
	}
	if n := r.openedIn("c1"); n != 1 {
		t.Fatalf("%d chat threads opened in c1, want 1", n)
	}
	owner.Close()
	for restart := 1; restart <= 2; restart++ {
		stop()
		stop, logged = runDaemonLog(t, r.root)
		eventually(t, "the supervisor's thread found again", 15*time.Second, func() bool {
			return strings.Contains(logged(), "supervisor's thread")
		})
		owner, err = wire.Dial(r.root)
		if err != nil {
			t.Fatal(err)
		}
		if n := supervisorThreads(t, owner, "c1"); n != 1 {
			t.Fatalf("after restart %d: %d supervisor threads in c1, want 1", restart, n)
		}
		all, _ := owner.Threads(context.Background(), control.Filter{})
		if len(all) != 1 || all[0].ID != th.ID {
			t.Fatalf("after restart %d: threads %+v, want only %d", restart, all, th.ID)
		}
		owner.Close()
	}
	if n := r.openedIn("c1"); n != 1 {
		t.Fatalf("%d chat threads opened in c1 after the restarts, want 1", n)
	}
	if n := r.openedIn("c2"); n != 0 {
		t.Fatalf("%d chat threads opened in c2, which names no supervisor", n)
	}
	stop()
}

// The supervisor's directory is its scope's, empty when its first agent
// starts, is no git repository, and is outside the workspaces directory,
// with no workspace in it, while an ordinary thread's workspace is where it
// was; nothing removes it, not even the removal of its closed thread.
func TestSupervisorDirectory(t *testing.T) {
	r, _ := svRoot(t, true)
	stop := runDaemon(t, r.root)
	defer stop()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx := context.Background()
	th := supervisorThread(t, owner, "c1", control.Idle)
	work, err := owner.Start(ctx, control.StartRequest{Key: "w", Scope: "c1", Thread: "tw", Repo: bareRepo(t), Input: control.Input{Text: "reply w"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := func(id control.ThreadID) string {
		var d string
		if err := dbOf(t, r.root).QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(id)).Scan(&d); err != nil || d == "" {
			t.Fatalf("thread %d has no directory: %v", id, err)
		}
		return d
	}
	svDir := dir(th.ID)
	if svDir != workspace.ScopePath(r.root, "c1") {
		t.Fatalf("the supervisor's directory is %s, want the scope's, %s", svDir, workspace.ScopePath(r.root, "c1"))
	}
	entries, err := os.ReadDir(svDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the supervisor's directory after its first turn: %v, %v; want empty", entries, err)
	}
	cmd := exec.Command("git", "-C", svDir, "rev-parse", "--git-dir")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CEILING_DIRECTORIES=" + filepath.Dir(r.root), "GIT_CONFIG_NOSYSTEM=1", "HOME=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the supervisor's directory is in a git repository: %s", out)
	}
	eventually(t, "the worker's workspace", 15*time.Second, func() bool {
		var d string
		dbOf(t, r.root).QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(work)).Scan(&d)
		return d != ""
	})
	workDir := dir(work)
	if workDir != workspace.Path(r.root, work) {
		t.Fatalf("the worker's workspace is %s, want %s", workDir, workspace.Path(r.root, work))
	}
	workspaces := filepath.Join(r.root, workspace.Dir)
	inside := func(p, d string) bool {
		rel, err := filepath.Rel(d, p)
		return err == nil && !strings.HasPrefix(rel, "..")
	}
	if inside(svDir, workspaces) || inside(workDir, svDir) || inside(svDir, workDir) {
		t.Fatalf("the supervisor's directory %s and the workspaces %s, %s overlap", svDir, workspaces, workDir)
	}

	// Closed and removed: the removal is refused, and the directory stays.
	os.WriteFile(filepath.Join(svDir, "notes.txt"), []byte("the supervisor's notes\n"), 0o600)
	n := strconv.FormatInt(int64(th.ID), 10)
	if code, _, errOut := cli(t, "close", "--root", r.root, n); code != 0 {
		t.Fatalf("close: %d %s", code, errOut)
	}
	if code, _, _ := cli(t, "remove", "--root", r.root, n); code == 0 {
		t.Fatal("the supervisor's directory was removed")
	}
	if b, err := os.ReadFile(filepath.Join(svDir, "notes.txt")); err != nil || string(b) != "the supervisor's notes\n" {
		t.Fatalf("the supervisor's notes after the removal: %q, %v", b, err)
	}
}

// An adapter that cannot start a confined session refuses: the supervisor's
// thread fails with that reason, and no agent process was started for it.
func TestSupervisorWithoutConfinementFails(t *testing.T) {
	r, conversations := svRoot(t, false)
	stop, logged := runDaemonLog(t, r.root)
	defer stop()
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	th := supervisorThread(t, owner, "c1", control.Failed)
	states := threadEvents(t, owner, th.ID, "state")
	last := states[len(states)-1]
	if last["to"] != "failed" || !strings.Contains(last["reason"].(string), "cannot start a confined session") {
		t.Fatalf("state events %v; want a failure because the agent cannot be confined", states)
	}
	if !strings.Contains(logged(), "cannot start a confined session") {
		t.Fatal("the daemon did not log why")
	}
	if entries, _ := os.ReadDir(conversations); len(entries) != 0 {
		t.Fatalf("the agent's program started %d sessions, want none", len(entries))
	}
	var sid int64
	dbOf(t, r.root).QueryRow(`SELECT id FROM sessions WHERE thread_id = ?`, int64(th.ID)).Scan(&sid)
	for _, f := range []string{host.HostPIDFile, host.PIDFile} {
		if _, err := os.Stat(filepath.Join(runner.StateDir(r.root, sid), f)); err == nil {
			t.Fatalf("%s exists: a session host or an agent was started", f)
		}
	}
	// An ordinary thread of the same scope is started as always.
	id, err := owner.Start(context.Background(), control.StartRequest{Key: "o", Scope: "c1", Thread: "to", Repo: bareRepo(t), Input: control.Input{Text: "reply ordinary"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ordinary thread's answer", 15*time.Second, func() bool {
		texts := threadEvents(t, owner, id, "text")
		return len(texts) > 0 && texts[len(texts)-1]["text"] == "ordinary"
	})

	// The failed thread stays across a restart. Once the owner closes it,
	// the next start replaces it, here by one that runs, since the agent
	// now takes a confined spec; a further restart starts no third.
	owner.Close()
	stop()
	stop, _ = runDaemonLog(t, r.root)
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if n := supervisorThreads(t, owner, "c1"); n != 1 {
		t.Fatalf("after a restart, %d supervisor threads; want the failed one only", n)
	}
	if code, _, errOut := cli(t, "close", "--root", r.root, strconv.FormatInt(int64(th.ID), 10)); code != 0 {
		t.Fatalf("close: %d %s", code, errOut)
	}
	owner.Close()
	stop()
	setConfig(t, r.root, func(cfg map[string]any) {
		cfg["adapters"].(map[string]any)[fake.Name].(map[string]any)["confines"] = true
	})
	for range 2 {
		stop, _ = runDaemonLog(t, r.root)
		owner, err = wire.Dial(r.root)
		if err != nil {
			t.Fatal(err)
		}
		next := supervisorThread(t, owner, "c1", control.Idle)
		if next.ID == th.ID || supervisorThreads(t, owner, "c1") != 2 {
			t.Fatalf("after closing the failed one: thread %d of %d; want one new one beside it", next.ID, supervisorThreads(t, owner, "c1"))
		}
		owner.Close()
		stop()
	}
}

// sendAndRead sends text to thread id as the owner and returns the agent's
// final answer to it.
func sendAndRead(t *testing.T, owner *wire.Client, id control.ThreadID, key, text string) string {
	t.Helper()
	before := len(threadEvents(t, owner, id, "turn_ended"))
	if err := owner.Send(context.Background(), id, key, control.Input{Text: text}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer to "+text, 15*time.Second, func() bool { return len(threadEvents(t, owner, id, "turn_ended")) > before })
	texts := threadEvents(t, owner, id, "text")
	return texts[len(texts)-1]["text"].(string)
}

// The supervisor's MCP server reaches the daemon with its session's token:
// it lists its own scope's threads only; after a daemon restart, during
// which the agent and its MCP server keep running, it still does; and after
// the session slept and resumed, with a new MCP server, it does again. The
// token is in no event, no stored spec and no log line.
func TestSupervisorToolsAcrossRestartAndSleep(t *testing.T) {
	r, _ := svRoot(t, true)
	stop, logged := runDaemonLog(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	th := supervisorThread(t, owner, "c1", control.Idle)
	work, err := owner.Start(ctx, control.StartRequest{Key: "w", Scope: "c1", Thread: "tw", Repo: bareRepo(t), Input: control.Input{Text: "reply w"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Start(ctx, control.StartRequest{Key: "x", Scope: "c2", Thread: "tx", Repo: bareRepo(t), Input: control.Input{Text: "reply x"}}); err != nil {
		t.Fatal(err)
	}
	listed := func(answer string) {
		t.Helper()
		if !strings.HasPrefix(answer, "mcp: 2 threads in this channel:") || !strings.Contains(answer, "#"+strconv.FormatInt(int64(work), 10)+" ") {
			t.Fatalf("list_threads answered %q; want c1's two threads", answer)
		}
	}
	listed(sendAndRead(t, owner, th.ID, "l1", "mcp list_threads"))
	if a := sendAndRead(t, owner, th.ID, "l2", "mcp read_thread {\"number\": 3}"); !strings.HasPrefix(a, "mcp error: not in this scope") {
		t.Fatalf("read_thread of c2's thread answered %q, want not in this scope", a)
	}

	var sid int64
	dbOf(t, r.root).QueryRow(`SELECT id FROM sessions WHERE thread_id = ?`, int64(th.ID)).Scan(&sid)
	token, err := os.ReadFile(filepath.Join(runner.StateDir(r.root, sid), supervisor.TokenFile))
	if err != nil || len(strings.TrimSpace(string(token))) < 40 {
		t.Fatalf("the session's token file: %q, %v", token, err)
	}
	pidFile := filepath.Join(runner.StateDir(r.root, sid), host.PIDFile)
	pid := func() string { b, _ := os.ReadFile(pidFile); return strings.TrimSpace(string(b)) }
	before := pid()

	// A restart: the agent and its MCP server keep running, with the token
	// they were started with.
	owner.Close()
	stop()
	stop, logged2 := runDaemonLog(t, r.root)
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	listed(sendAndRead(t, owner, th.ID, "l3", "mcp list_threads"))
	if pid() != before {
		t.Fatalf("the agent was started again (%s, then %s): the restart did not keep it", before, pid())
	}

	// Asleep, then woken by a message: a new agent and MCP server.
	owner.Close()
	stop()
	setConfig(t, r.root, func(cfg map[string]any) { cfg["idle"] = "1s" })
	stop, logged3 := runDaemonLog(t, r.root)
	defer stop()
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	supervisorThread(t, owner, "c1", control.Asleep)
	listed(sendAndRead(t, owner, th.ID, "l4", "mcp list_threads"))
	if pid() == before {
		t.Fatal("the agent was not started again after it slept")
	}

	secret := strings.TrimSpace(string(token))
	for name, l := range map[string]string{"first": logged(), "second": logged2(), "third": logged3()} {
		if strings.Contains(l, secret) {
			t.Fatalf("the %s daemon's log has the token", name)
		}
	}
	rows, err := dbOf(t, r.root).Query(`SELECT data FROM events UNION ALL SELECT spec FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		if strings.Contains(s, secret) {
			t.Fatalf("the store has the token: %s", s)
		}
	}
	var spec string
	dbOf(t, r.root).QueryRow(`SELECT spec FROM sessions WHERE id = ?`, sid).Scan(&spec)
	if !strings.Contains(spec, `"Confine":{`) {
		t.Fatalf("the supervisor's stored spec %s keeps no confinement", spec)
	}
	dbOf(t, r.root).QueryRow(`SELECT spec FROM sessions WHERE thread_id = ?`, int64(work)).Scan(&spec)
	if strings.Contains(spec, "Confine") {
		t.Fatalf("an ordinary thread's stored spec %s has a confinement", spec)
	}
}
