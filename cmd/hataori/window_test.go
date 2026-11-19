package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/window"
	"github.com/yousan/hataori/internal/wire"
)

// configSnapshot is every file and directory under dir: its path, mode, time and
// content.
func configSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v %d %d\n", p, st.Mode(), st.Size(), st.ModTime().UnixNano())
		if st.Mode().IsRegular() {
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b.Write(c)
			b.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// `hataori plugin` writes a plugin and a marketplace under the root, prints
// the two commands that install it, and writes nothing into a Claude Code
// configuration directory.
func TestPluginCommand(t *testing.T) {
	root := newRoot(t)
	abs, _ := filepath.Abs(root)
	config, home := t.TempDir(), t.TempDir()
	for p, c := range map[string]string{
		filepath.Join(config, "settings.json"):                      `{"enabledPlugins":{}}`,
		filepath.Join(config, "plugins", "known_marketplaces.json"): `{}`,
		filepath.Join(home, ".claude.json"):                         `{}`,
		filepath.Join(home, ".claude", "settings.json"):             `{}`,
	} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("HOME", home)
	before := configSnapshot(t, config) + configSnapshot(t, home)
	time.Sleep(10 * time.Millisecond) // a file written now would have another time

	want := fmt.Sprintf("claude plugin marketplace add '%s'\nclaude plugin install %s@hataori-%s\n",
		filepath.Join(abs, PluginDir), claudecode.PluginName, instanceTag(abs))
	for range 2 { // writing it again changes nothing
		var out, errOut bytes.Buffer
		if code := run([]string{"plugin", "--root", root}, &out, &errOut); code != 0 || out.String() != want || errOut.Len() != 0 {
			t.Fatalf("exit %d, printed:\n%s\nwant:\n%s\n%s", code, out.String(), want, errOut.String())
		}
	}
	if after := configSnapshot(t, config) + configSnapshot(t, home); after != before {
		t.Fatalf("hataori plugin changed a Claude Code configuration directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	exe, _ := os.Executable()
	hooks, err := os.ReadFile(filepath.Join(abs, PluginDir, claudecode.PluginHooksFile))
	if err != nil {
		t.Fatal(err)
	}
	if cmd := fmt.Sprintf("'%s' hook --window --root '%s'", exe, abs); !strings.Contains(string(hooks), cmd) {
		t.Fatalf("hooks.json does not run %s:\n%s", cmd, hooks)
	}
	for _, f := range []string{claudecode.MarketplaceFile, claudecode.PluginFile} {
		if _, err := os.Stat(filepath.Join(abs, PluginDir, f)); err != nil {
			t.Fatal(err)
		}
	}
}

// fixedTerms lists the same terminals every time.
type fixedTerms []control.HostTerminal

func (f fixedTerms) List(context.Context, []int) ([]control.HostTerminal, error) {
	return slices.Clone(f), nil
}

func (f fixedTerms) Screen(context.Context, string, []int) (string, bool, error) {
	return "", false, nil
}

func (f fixedTerms) View(context.Context, string, []int) (control.HostView, bool, error) {
	return control.HostView{}, false, nil
}

// The reports of a session a person started reach HostTerminals and
// nothing else: not the event log, which a supervisor's wake-up and a
// plugin's events are read from, and no file under the root, where the
// database and the chat outbox are. A session that is a thread's names it.
func TestWindowReportsStayInMemory(t *testing.T) {
	hostTermsSource = fixedTerms{{Device: "/dev/pts/9", Process: "claude", Tmux: &control.TmuxPane{Pane: "%7", Session: "work", Window: "0", Index: "0"}}}
	t.Cleanup(func() { hostTermsSource = nil })
	root := newRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = true })
	startDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread's turn", 30*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Idle && d.Turns == 1
	})

	const marker = "MARKER-2c9e-reported"
	r := window.Report{Holder: window.Tmux, Server: "/tmp/tmux-1000/default,4242", Pane: "%7", Device: "/dev/pts/9", PID: os.Getpid(), // a process that exists
		Agent: claudecode.Name, Session: marker + "-session", Dir: "/srv/" + marker, ConfigDir: "/home/" + marker,
		Model: marker + "-model", Interactive: true}
	reported := func(event string) *control.ReportedSession {
		t.Helper()
		r.Event = event
		if err := window.Send(ctx, root, r); err != nil {
			t.Fatal(err)
		}
		var got *control.ReportedSession
		eventually(t, "the report of "+event, 10*time.Second, func() bool {
			ts, err := c.HostTerminals(ctx)
			if err != nil || len(ts) != 1 {
				return false
			}
			got = ts[0].Reported
			return got != nil && got.Event == event && got.Session == r.Session
		})
		return got
	}
	got := reported("SessionStart")
	if got.Window != "tmux:/tmp/tmux-1000/default,4242,%7" || got.Agent != claudecode.Name || got.Dir != r.Dir || got.At.IsZero() || got.Thread != nil {
		t.Fatalf("reported %+v", got)
	}
	reported("UserPromptSubmit")

	// Nothing of the reports in the event log.
	evs, err := c.Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for e := range evs {
		if e.Kind == control.CaughtUp {
			break
		}
		n++
		if b, _ := json.Marshal(e); bytes.Contains(b, []byte(marker)) {
			t.Fatalf("an event holds a report: %s", b)
		}
	}
	if n == 0 {
		t.Fatal("no event at all: the check proves nothing")
	}
	// Nor in any file under the root: the database, its journal, the chat
	// outbox, the sessions' state.
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(marker)) {
				t.Errorf("%s holds a report", p)
			}
		}
		return nil
	})

	// A thread's session, opened again in a person's window, names its
	// thread.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, store.File)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT agent, agent_session_id FROM sessions WHERE thread_id = ?`, int64(id)).Scan(&r.Agent, &r.Session); err != nil || r.Session == "" {
		t.Fatalf("the thread's session: %q, %v", r.Session, err)
	}
	if got := reported("SessionStart"); got.Thread == nil || *got.Thread != id {
		t.Fatalf("the thread's session reported %+v", got)
	}
}
