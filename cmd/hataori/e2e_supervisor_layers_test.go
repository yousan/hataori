package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

var updateLayers = flag.Bool("update-layers", false, "write the golden files of a supervisor's composed instructions")

// instructionsOf returns the instructions the last launch of thread sv's
// newest session was given, as the fake adapter wrote them.
func instructionsOf(t *testing.T, root string, sv control.ThreadID) string {
	t.Helper()
	var session int64
	if err := dbOf(t, root).QueryRow(`SELECT id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, int64(sv)).Scan(&session); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(runner.StateDir(root, session), fake.InstructionsFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// golden compares got with testdata/supervisor-layers/name, or writes it
// with -update-layers.
func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "supervisor-layers", name)
	if *updateLayers {
		os.MkdirAll(filepath.Dir(file), 0o755)
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("the supervisor's instructions differ from %s:\n%s", file, got)
	}
}

// userFiles writes the user's files into scope c1's directory, before the
// daemon makes it.
func userFiles(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	dir := workspace.ScopePath(root, "c1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A launch of the supervisor's session gives hataori's layer, then the
// config's, then AGENTS.md, SOUL.md, USER.md and IDENTITY.md, in that
// order, each under its name; without any of the files, the first two
// only.
func TestSupervisorInstructionsInLayers(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
	}{
		{"files.md", map[string]string{
			"IDENTITY.md": "Your name is Tsumugi.\n",
			"USER.md":     "The people here read Japanese.\n",
			"SOUL.md":     "Be brief.\n",
			"AGENTS.md":   "# Orders\n\nOne issue per thread.\n",
		}},
		{"none.md", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := svRoot(t, true)
			if c.files != nil {
				userFiles(t, r.root, c.files)
			}
			stop := runDaemon(t, r.root)
			defer stop()
			owner, err := wire.Dial(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			sv := supervisorThread(t, owner, "c1", control.Idle)
			got := instructionsOf(t, r.root, sv.ID)
			if !strings.HasPrefix(got, supervisor.Layer+"\n\n"+supervisor.ConfigLayer(supervisor.Scope{Instructions: "Route the channel's work.",
				Limits: supervisor.Limits{Turns: 12, Sends: 20, Tokens: 2_000_000}, RemindersMax: 10}, nil)) {
				t.Fatalf("the instructions do not start with hataori's layer and the config's:\n%s", got)
			}
			if c.files == nil && strings.Contains(got, "# The user's files") {
				t.Fatalf("a user's layer without any of the files:\n%s", got)
			}
			golden(t, c.name, got)
		})
	}
}

// filesNotices returns the lines in the chat thread conv that name the
// user's files a turn of the supervisor's wrote.
func (wt *wakeTest) filesNotices(conv surface.Conversation) []string {
	var out []string
	for _, c := range wt.r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && strings.HasPrefix(c.Message.Text, "Changed by ") {
			out = append(out, c.Message.Text)
		}
	}
	return out
}

// told waits until no write of scope c1's is left whose line was not posted.
func (wt *wakeTest) told(t *testing.T) {
	t.Helper()
	eventually(t, "every write to be told", 10*time.Second, func() bool {
		var n int
		return wt.db.QueryRow(`SELECT count(*) FROM supervisor_writes WHERE scope = 'c1'`).Scan(&n) == nil && n == 0
	})
}

// writes has thread id run script, of calls of the tools of files (toolScript),
// in a turn of its own. Each call must write.
func writes(t *testing.T, owner *wire.Client, id control.ThreadID, key string, script ...string) {
	t.Helper()
	before := len(threadEvents(t, owner, id, "text"))
	sendAndRead(t, owner, id, key, strings.Join(script, "\n"))
	texts := threadEvents(t, owner, id, "text")[before:]
	for i, e := range texts {
		if s := e["text"].(string); !strings.HasPrefix(s, "mcp: Written: ") {
			t.Fatalf("answer %d to %q: %q", i+1, script, s)
		}
	}
	if len(texts) != len(script) {
		t.Fatalf("%d answers to %q", len(texts), script)
	}
}

// toolScript is the fake agent's script that calls tool with args.
func toolScript(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return "mcp " + tool + " " + string(b)
}

// A user's file changed on the host between two wakes is named in the
// second wake and not in the third, nor in a line of the thread. A turn of
// the supervisor that writes some of them gets one line in its thread
// naming each once, also when the daemon died after posting it and before
// recording that; a turn that writes none gets no line, whatever changed
// on the host meanwhile, and neither does a write that did not fit.
func TestSupervisorUserFileChanges(t *testing.T) {
	wt := newWakeTest(t)
	dir := userFiles(t, wt.r.root, map[string]string{"USER.md": "The people here read Japanese.\n"})
	stop, owner, sv := wt.start(t)
	defer func() { stop() }()
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}

	wake := func(n int, key string) string {
		t.Helper()
		_, pos := wt.worker(t, owner, "c1", key, "reply "+key)
		wt.read(t, pos)
		wt.quiet(t)
		got := wt.woken(t, owner, sv.ID, n)
		wt.read(t, wt.ended(t, sv.ID, n+1))
		return got[n-1]
	}
	if w := wake(1, "a"); strings.Contains(w, "USER.md") {
		t.Fatalf("the first wake names USER.md, which did not change:\n%s", w)
	}
	os.WriteFile(filepath.Join(dir, "USER.md"), []byte("The people here read English.\n"), 0o600)
	if w := wake(2, "b"); !strings.Contains(w, "\n"+supervisor.FilesChanged([]string{"USER.md"})+"\n") {
		t.Fatalf("the second wake does not name USER.md, changed since the first:\n%s", w)
	}
	if w := wake(3, "c"); strings.Contains(w, "USER.md") {
		t.Fatalf("the third wake names USER.md again:\n%s", w)
	}

	// A turn that writes nothing, then one that writes SOUL.md, after which
	// the daemon dies, having posted the line and before recording that.
	// Both turn ends are read again after the restart: the line is posted
	// once.
	killed := func(step string) error {
		if step == "files told" {
			return errors.New("killed")
		}
		return nil
	}
	stop()
	wt.hook.Store(&killed)
	stop, owner, sv = wt.start(t)
	if a := sendAndRead(t, owner, sv.ID, "p", "reply nothing yet"); a != "nothing yet" {
		t.Fatalf("the supervisor's turn: %q", a)
	}
	wt.read(t, wt.ended(t, sv.ID, wt.turns(t, sv.ID)))
	writes(t, owner, sv.ID, "s", toolScript(t, supervisor.ToolWrite, map[string]any{"path": "SOUL.md", "text": "Light reports.\n"}))
	eventually(t, "the line naming SOUL.md", 10*time.Second, func() bool { return len(wt.filesNotices(conv)) == 1 })
	var left int
	if err := wt.db.QueryRow(`SELECT count(*) FROM supervisor_writes`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("%d writes not recorded as told at the kill, %v; want 1", left, err)
	}
	stop()
	// While the daemon is down, USER.md is edited on the host: no turn of the
	// supervisor's wrote it, and no line names it.
	os.WriteFile(filepath.Join(dir, "USER.md"), []byte("The people here read Spanish.\n"), 0o600)
	stop, owner, sv = wt.start(t)
	wt.told(t)
	if a := sendAndRead(t, owner, sv.ID, "q", "reply nothing written"); a != "nothing written" {
		t.Fatalf("the supervisor's turn: %q", a)
	}
	// An edit that does not fit writes nothing; one that does has its line;
	// a turn that writes several has one line, which names each once and none
	// that is not one of the user's.
	if a := sendAndRead(t, owner, sv.ID, "m", toolScript(t, supervisor.ToolEdit, map[string]any{"path": "USER.md", "old": "English", "new": "French"})); !strings.HasPrefix(a, "mcp: Nothing was written: the passage is not in the file. ") || !strings.Contains(a, "read Spanish.") {
		t.Fatalf("an edit of a passage that is not there: %q", a)
	}
	writes(t, owner, sv.ID, "u", toolScript(t, supervisor.ToolEdit, map[string]any{"path": "USER.md", "old": "Spanish", "new": "French"}))
	var script []string
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{supervisor.ToolWrite, map[string]any{"path": "notes/plan.md", "text": "a plan\n"}},
		{supervisor.ToolEdit, map[string]any{"path": "SOUL.md", "old": "Light", "new": "Short"}},
		{supervisor.ToolWrite, map[string]any{"path": "AGENTS.md", "text": "One issue per thread.\n"}},
		{supervisor.ToolEdit, map[string]any{"path": "AGENTS.md", "old": "One issue", "new": "Two issues"}},
	} {
		script = append(script, toolScript(t, c.tool, c.args))
	}
	writes(t, owner, sv.ID, "v", script...)
	writes(t, owner, sv.ID, "n", toolScript(t, supervisor.ToolWrite, map[string]any{"path": "notes/more.md", "text": "more\n"}))
	wt.read(t, wt.ended(t, sv.ID, wt.turns(t, sv.ID)))
	wt.told(t)
	time.Sleep(300 * time.Millisecond) // for the outbox to post a line, were there one
	line := func(names string) string {
		return fmt.Sprintf("Changed by this turn, in the supervisor's instructions: %s.", names)
	}
	want := []string{line("SOUL.md"), line("USER.md"), line("AGENTS.md and SOUL.md")}
	if n := wt.filesNotices(conv); strings.Join(n, "|") != strings.Join(want, "|") {
		t.Fatalf("the lines naming the files a turn wrote: %q; want %q", n, want)
	}
	for name, want := range map[string]string{"SOUL.md": "Short reports.\n", "USER.md": "The people here read French.\n",
		"AGENTS.md": "Two issues per thread.\n", "notes/plan.md": "a plan\n"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); string(b) != want {
			t.Errorf("%s holds %q, %v; want %q", name, b, err, want)
		}
	}
}

// A turn of a conversation that writes one of the user's files is named in
// its own chat thread and in the supervisor's own, and in no other
// conversation's (#868). What a session wrote after its turn's end, as a
// call that an interrupted turn left running does, is named with the next
// one's, or when the conversation is closed.
func TestConversationsUserFileChanges(t *testing.T) {
	f := newSvTalks(t, "")
	a, b := f.talk(t, "a"), f.talk(t, "b")
	writes(t, f.owner, a, "wa", toolScript(t, supervisor.ToolWrite, map[string]any{"path": "AGENTS.md", "text": "One issue per thread.\n"}))
	writes(t, f.owner, b, "wb", toolScript(t, supervisor.ToolEdit, map[string]any{"path": "AGENTS.md", "old": "One issue", "new": "Two issues"}))
	f.wt.told(t)

	c := f.talk(t, "c")
	if out := mcpCall(t, f.tools(t, c), supervisor.ToolWrite, map[string]any{"path": "SOUL.md", "text": "Be brief.\n"}); out.IsError || !strings.HasPrefix(out.Text, "Written: ") {
		t.Fatalf("the third conversation's write: %+v", out)
	}
	f.wt.read(t, f.wt.ended(t, c, 1))
	time.Sleep(300 * time.Millisecond) // for the outbox to post a line, were there one
	if n := f.wt.filesNotices(f.chat("c")); len(n) != 0 {
		t.Fatalf("a line with no turn's end: %q", n)
	}
	closeThread(t, f.wt.r.root, c)
	f.wt.told(t)
	time.Sleep(300 * time.Millisecond) // for the outbox to post a line, were there one

	const in = "in the supervisor's instructions: "
	for where, want := range map[surface.Conversation][]string{
		f.chat("a"): {"Changed by this turn, " + in + "AGENTS.md."},
		f.chat("b"): {"Changed by this turn, " + in + "AGENTS.md."},
		f.chat("c"): {"Changed by this turn, " + in + "SOUL.md."},
		f.conv: {
			fmt.Sprintf("Changed by a turn of thread #%d, %sAGENTS.md.", a, in),
			fmt.Sprintf("Changed by a turn of thread #%d, %sAGENTS.md.", b, in),
			fmt.Sprintf("Changed by a turn of thread #%d, %sSOUL.md.", c, in),
		},
	} {
		if got := f.wt.filesNotices(where); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("the lines in %v: %q; want %q", where, got, want)
		}
	}
}

// A repository that no forge serves is named as such in the instructions of
// the launches, and no longer in the next launch's after a forge was added
// to the config and the daemon restarted; until then a read of it is
// refused (#795).
func TestSupervisorLayerNamesUnservedRepos(t *testing.T) {
	fillFakeForge()
	wt := newWakeTest(t, func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["repos"] = []string{forgeAddress}
	})
	const unserved = "No forge answers for acme/widgets"
	stop, owner, sv := wt.start(t)
	if got := instructionsOf(t, wt.r.root, sv.ID); !strings.Contains(got, unserved) {
		t.Fatalf("no forge serves the repository, and the instructions do not say so:\n%s", got)
	}
	const read = `mcp list_issues {"repo": "acme/widgets"}`
	if a := sendAndRead(t, owner, sv.ID, "r1", read); !strings.Contains(a, "no forge of this instance (forges) serves it") {
		t.Fatalf("list_issues of it: %s", a)
	}
	owner.Close()
	stop()

	if err := os.WriteFile(filepath.Join(wt.r.root, "config", "forge-token"), []byte(fakeForgeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changeConfig(t, wt.r.root, func(cfg map[string]any) {
		cfg["forges"] = map[string]any{fakeForgeName: map[string]any{"token_file": "forge-token"}}
	})
	stop, owner, sv = wt.start(t)
	defer stop()
	_, pos := wt.worker(t, owner, "c1", "a", "reply a")
	wt.read(t, pos)
	wt.quiet(t)
	wt.woken(t, owner, sv.ID, 1) // a launch of its own
	if got := instructionsOf(t, wt.r.root, sv.ID); strings.Contains(got, unserved) || !strings.Contains(got, "- repos: acme/widgets. The only repositories you may start a thread in, and read on their forge.\n") {
		t.Fatalf("a forge serves the repository; the next launch's instructions:\n%s", got)
	}
	if a := sendAndRead(t, owner, sv.ID, "r2", read); !strings.Contains(a, "Issues of acme/widgets") {
		t.Fatalf("list_issues of it, once served: %s", a)
	}
}
