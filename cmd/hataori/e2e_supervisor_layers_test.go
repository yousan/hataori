package main

import (
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

// filesNotices returns the lines in the supervisor's thread that name the
// user's files a turn changed.
func (wt *wakeTest) filesNotices(sv control.ThreadInfo) []string {
	var out []string
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	for _, c := range wt.r.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && strings.HasPrefix(c.Message.Text, "Changed by the end of this turn") {
			out = append(out, c.Message.Text)
		}
	}
	return out
}

// looked waits until the waker looked at the user's files after the
// supervisor's turn end at pos, and recorded them.
func (wt *wakeTest) looked(t *testing.T, pos uint64) {
	t.Helper()
	eventually(t, fmt.Sprintf("the files seen after the turn end at %d", pos), 10*time.Second, func() bool {
		var at int64
		return wt.db.QueryRow(`SELECT files_pos FROM supervisor_wakes WHERE scope = 'c1'`).Scan(&at) == nil && uint64(at) >= pos
	})
}

// A user's file changed on the host between two wakes is named in the
// second wake and not in the third, nor in a line of the thread. A turn of
// the supervisor that changes one gets one line in its thread naming it,
// once, also when the daemon died after posting it and before recording
// it; a turn that changes none gets no line.
func TestSupervisorUserFileChanges(t *testing.T) {
	wt := newWakeTest(t)
	dir := userFiles(t, wt.r.root, map[string]string{"USER.md": "The people here read Japanese.\n"})
	stop, owner, sv := wt.start(t)
	defer func() { stop() }()

	wake := func(n int, key string) string {
		t.Helper()
		_, pos := wt.worker(t, owner, "c1", key, "reply "+key)
		wt.read(t, pos)
		wt.quiet(t)
		got := wt.woken(t, owner, sv.ID, n)
		wt.looked(t, wt.ended(t, sv.ID, n+1))
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
	if n := wt.filesNotices(sv); len(n) != 0 {
		t.Fatalf("lines for a change no turn of the supervisor made: %q", n)
	}

	// A turn that changes nothing, then one that changes SOUL.md, after
	// which the daemon dies, having posted the line and before recording
	// it. Both turn ends are read again after the restart: the first names
	// nothing, though SOUL.md differs by then, and the second's line is
	// posted once.
	killed := func(step string) error {
		if step == "files told" {
			return errors.New("killed")
		}
		return nil
	}
	stop()
	wt.hook.Store(&killed)
	stop, owner, sv = wt.start(t)
	turn := func(key, text, want string) {
		t.Helper()
		if a := sendAndRead(t, owner, sv.ID, key, text); a != want {
			t.Fatalf("the supervisor's turn %q: %q", text, a)
		}
	}
	turn("p", "reply nothing yet", "nothing yet")
	wt.looked(t, wt.ended(t, sv.ID, wt.turns(t, sv.ID)))
	turn("s", "write SOUL.md Light reports.", "wrote SOUL.md")
	eventually(t, "the line naming SOUL.md", 10*time.Second, func() bool { return len(wt.filesNotices(sv)) == 1 })
	stop()
	// While the daemon is down, USER.md is edited on the host: the line
	// posted for SOUL.md's turn does not name it, so the next turn's does.
	os.WriteFile(filepath.Join(dir, "USER.md"), []byte("The people here read Spanish.\n"), 0o600)
	stop, owner, sv = wt.start(t)
	// Then a turn that changes nothing, one that changes USER.md, and one
	// that puts it back as the last wake told it.
	turn("q", "reply nothing changed", "nothing changed")
	turn("u", "write USER.md The people here read French.", "wrote USER.md")
	turn("v", "write USER.md The people here read English.", "wrote USER.md")
	wt.looked(t, wt.ended(t, sv.ID, wt.turns(t, sv.ID)))
	time.Sleep(300 * time.Millisecond) // for the outbox to post a line, were there one
	line := func(name string) string {
		return fmt.Sprintf("Changed by the end of this turn, in the supervisor's instructions: %s.", name)
	}
	want := []string{line("SOUL.md"), line("USER.md"), line("USER.md"), line("USER.md")}
	if n := wt.filesNotices(sv); strings.Join(n, "|") != strings.Join(want, "|") {
		t.Fatalf("the lines naming the files a turn changed: %q; want %q", n, want)
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
