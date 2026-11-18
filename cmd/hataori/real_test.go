package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/workspace"
)

// TestRealClaudeCode checks phase 1 against the real Claude Code CLI: the
// daemon as a process of its own, the registered Claude Code adapter, and
// the fake chat surface. It runs only when asked for (docs/testing.md):
//
//	HATAORI_REAL_CLAUDE=1 go test -run TestRealClaudeCode -v -timeout 50m ./cmd/hataori
//
// The CLI runs as on a person's machine, logged in however it is: the check
// passes it nothing about that and looks at none of it. It runs the CLI
// through a wrapper that keeps the person's own context out, as the
// captures do: --setting-sources project,local (the person's settings,
// hooks and plugins are not loaded; the adapter's --settings holds only its
// hooks), no CLAUDE.md, no auto-memory, no connectors, the model haiku. The
// workspace is a new clone in a new temporary directory. The wrapper adds
// the full-screen interface to the adapter's --settings, as a person's
// settings may: with it the CLI reports SessionStart before it reads keys
// (#178). The long turn waits on a command, so the model is asked for
// little.
func TestRealClaudeCode(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run the check against the real CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ver, err := exec.CommandContext(ctx, "claude", "--version").Output()
	cancel()
	if err != nil {
		t.Fatalf("claude --version: %v", err)
	}
	t.Logf("CLI %s", strings.TrimSpace(string(ver)))
	settingsBefore := userSettings(t)

	bin := t.TempDir()
	wrapper := filepath.Join(bin, "claude-for-the-check")
	if err := os.WriteFile(wrapper, []byte(realWrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	p := newProcRoot(t, func(cfg map[string]any) {
		cfg["model"] = "haiku"
		cfg["login_relay"] = false
		cfg["adapters"] = map[string]any{claudecode.Name: map[string]any{"command": wrapper}}
	})
	p.deadline = time.Now().Add(40 * time.Minute)
	p.start(t)
	repo := bareRepo(t)

	var th surface.Conversation
	ok := true
	step := func(name string, f func(t *testing.T)) {
		if !ok {
			t.Run(name, func(t *testing.T) { t.Skip("an earlier step failed") })
			return
		}
		if ok = t.Run(name, f); !ok {
			p.dump(t, th)
		}
	}
	// turn posts text into the thread and waits until its turn has ended
	// and the agent answered with word.
	turn := func(t *testing.T, text, word string, timeout time.Duration) {
		t.Helper()
		before := len(p.turnsEnded(t, th))
		p.in(th, text)
		eventually(t, "the end of the turn", p.within(timeout), func() bool { return len(p.turnsEnded(t, th)) > before })
		eventually(t, "the answer "+word, p.within(15*time.Second), func() bool { return p.replies(th, word) > 0 })
	}
	// bash waits until a Bash command starts in the thread, the turn still
	// running.
	bash := func(t *testing.T, tools int) {
		t.Helper()
		eventually(t, "the command", p.within(3*time.Minute), func() bool {
			started := p.list(t, "tool_started", th)
			for _, e := range started[min(tools, len(started)):] {
				if e.Data["tool"] == "Bash" {
					return true
				}
			}
			return false
		})
	}

	step("1 first start in a new clone", func(t *testing.T) {
		p.in(c1, "!go repo:"+repo+" Reply with just the word: ok")
		th = p.thread(t, 0)
		eventually(t, "the answer ok", p.within(3*time.Minute), func() bool {
			return len(p.turnsEnded(t, th)) == 1 && p.replies(th, "ok") > 0
		})
		tty, _ := os.ReadFile(filepath.Join(runner.StateDir(p.root, 1), host.TerminalLog))
		if !bytes.Contains(tty, []byte("trust")) || !bytes.Contains(tty, []byte("folder")) {
			t.Fatal("the terminal showed no folder-trust dialog: the clone was not new to the CLI")
		}
		if !bytes.Contains(tty, []byte("\x1b[?1049h")) {
			t.Fatal("no full-screen interface: the CLI did not take tui from --settings")
		}
		if bytes.Contains(tty, []byte("^[[200~")) {
			t.Fatal("the terminal echoed the paste: it was written before the CLI read keys")
		}
	})
	step("2 a turn", func(t *testing.T) {
		ended := p.turnsEnded(t, th)
		if len(ended) != 1 || ended[0].Data["outcome"] != "completion" || ended[0].Data["by"] != "caller" {
			t.Fatalf("turn_ended %v; want one completion by the caller", ended)
		}
	})
	var session string
	step("3 a follow-up with the same session id", func(t *testing.T) {
		_, session = p.agentSession(t)
		turn(t, "Reply with just the word: again", "again", 3*time.Minute)
		if n, id := p.agentSession(t); n != 1 || id != session || id == "" {
			t.Fatalf("%d sessions, agent session %q then %q; want the same", n, session, id)
		}
	})
	step("4 a question and its answer", func(t *testing.T) {
		before := len(p.turnsEnded(t, th))
		p.in(th, "Use the AskUserQuestion tool once to ask me: 'Which colour?' with the two options red and blue. "+
			"Then reply with just the colour I chose.")
		eventually(t, "the question in the thread", p.within(3*time.Minute), func() bool {
			for _, c := range p.driver.Calls(th) {
				if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "Which colour?") {
					return true
				}
			}
			return false
		})
		eventually(t, "the blocked turn", p.within(time.Minute), func() bool {
			e := p.turnsEnded(t, th)
			return len(e) == before+1 && e[before].Data["outcome"] == "blocked"
		})
		p.in(th, "blue")
		eventually(t, "the answer blue", p.within(3*time.Minute), func() bool {
			return len(p.turnsEnded(t, th)) > before+1 && p.replies(th, "blue") > 0
		})
		if a := p.list(t, "answer", th); len(a) != 1 || !strings.Contains(fmt.Sprint(a[0].Data["text"]), "blue") {
			t.Fatalf("answer events %v; want one with blue", a)
		}
	})
	step("5 an interrupt", func(t *testing.T) {
		before := len(p.turnsEnded(t, th))
		tools := len(p.events(t, "tool_started", th))
		p.in(th, "Run this Bash command in the foreground, with a timeout of 300000 ms: "+
			"python3 -c 'import time; time.sleep(120)'. Then reply with just: slept")
		bash(t, tools)
		if code, _, errOut := cli(t, "stop", "--root", p.root, th.Thread); code != 0 {
			t.Fatalf("stop: exit %d, %s", code, errOut)
		}
		var ended []realEvent
		eventually(t, "the end of the interrupted turn", p.within(time.Minute), func() bool {
			ended = p.turnsEnded(t, th)
			return len(ended) > before
		})
		if e := ended[before]; e.Data["outcome"] != "failure" || e.Data["reason"] != "interrupted" {
			t.Fatalf("turn_ended %v; want a failure, interrupted", e.Data)
		}
	})
	step("6 a turn of 10 minutes", func(t *testing.T) {
		quiet(t, p, th)
		before := len(p.turnsEnded(t, th))
		turn(t, "Run these two Bash commands one after the other, each in the foreground with a timeout of 400000 ms, "+
			"never in the background: python3 -c 'import time; time.sleep(300)' and then "+
			"python3 -c 'import time; time.sleep(305)'. Then reply with just: long done", "long done", 20*time.Minute)
		ended, started := p.turnsEnded(t, th), p.list(t, "turn_started", th)
		if len(ended) != before+1 || len(started) != len(ended) || ended[before].Data["outcome"] != "completion" {
			t.Fatalf("turn_started %d, turn_ended %v", len(started), ended)
		}
		took := ended[before].Time.Sub(started[before].Time)
		t.Logf("the turn took %v", took.Round(time.Second))
		if took < 10*time.Minute {
			t.Fatalf("the turn took %v", took)
		}
	})
	step("7 a turn the agent starts by itself", func(t *testing.T) {
		before := len(p.turnsEnded(t, th))
		turn(t, "Run the Bash command `sleep 45` with run_in_background set to true. Do not wait for it. "+
			"Reply with just: started", "started", 3*time.Minute)
		var agentTurns []realEvent
		eventually(t, "a turn the agent started, ended with no task running", p.within(4*time.Minute), func() bool {
			agentTurns = nil
			for _, e := range p.turnsEnded(t, th)[before:] {
				if e.Data["by"] == "agent" {
					agentTurns = append(agentTurns, e)
				}
			}
			return len(agentTurns) > 0 && agentTurns[len(agentTurns)-1].Data["background"] == 0.0
		})
		ended := p.turnsEnded(t, th)
		if ended[before].Data["by"] != "caller" || ended[before].Data["background"] != 1.0 {
			t.Fatalf("turn_ended %v; want the caller's first, with its task running", ended[before:])
		}
		if len(agentTurns) != 1 || agentTurns[0].Data["outcome"] != "completion" {
			t.Fatalf("turns the agent started: %v; want one completion", agentTurns)
		}
	})
	step("8 killing the daemon during a turn", func(t *testing.T) {
		quiet(t, p, th)
		before := len(p.turnsEnded(t, th))
		tools := len(p.events(t, "tool_started", th))
		toolsEnded := len(p.events(t, "tool_ended", th))
		p.in(th, "Run this Bash command in the foreground, with a timeout of 300000 ms: "+
			"python3 -c 'import time; time.sleep(40)'. Then reply with just: after the kill")
		bash(t, tools)
		p.kill(t)
		// The daemon is down: its events are read from the store.
		stored := func(kind string) int {
			var n int
			p.db(t).QueryRow(`SELECT count(*) FROM events WHERE thread = ? AND kind = ?`, th.Thread, kind).Scan(&n)
			return n
		}
		if stored("tool_ended") != toolsEnded || stored("turn_ended") != before {
			t.Fatal("the command or the turn ended before the daemon was killed; the kill was not during the turn")
		}
		p.start(t)
		eventually(t, "the answer after the kill", p.within(3*time.Minute), func() bool {
			return len(p.turnsEnded(t, th)) > before && p.replies(th, "after the kill") > 0
		})
		time.Sleep(5 * time.Second)
		if n := p.replies(th, "after the kill"); n != 1 {
			t.Fatalf("the answer was posted %d times", n)
		}
		if n := len(p.turnsEnded(t, th)); n != before+1 {
			t.Fatalf("%d turns ended, want %d", n, before+1)
		}
	})
	step("9 sleep and resume", func(t *testing.T) {
		turn(t, "Remember this word for later: heron. Reply with just: remembered", "remembered", 3*time.Minute)
		quiet(t, p, th)
		// A short idle period, for this step only: the daemon reads it at start.
		path := filepath.Join(p.root, instance.ConfigFile)
		b, _ := os.ReadFile(path)
		short := bytes.Replace(b, []byte(`"idle":"1h"`), []byte(`"idle":"20s"`), 1)
		if bytes.Equal(short, b) {
			t.Fatal("the config has no idle period of 1h to shorten")
		}
		if err := os.WriteFile(path, short, 0o600); err != nil {
			t.Fatal(err)
		}
		pid := p.agentPID(t)
		p.restart(t)
		eventually(t, "the agent to be put to sleep for being idle", p.within(3*time.Minute), func() bool {
			for _, e := range p.list(t, "state", th) {
				if e.Data["to"] == "asleep" && e.Data["reason"] == "idle" {
					return !alive(pid)
				}
			}
			return false
		})
		turn(t, "Which word did I ask you to remember? Reply with just that word.", "heron", 3*time.Minute)
		if n, id := p.agentSession(t); n != 1 || id != session {
			t.Fatalf("%d sessions, agent session %q then %q; want the same conversation", n, session, id)
		}
	})

	// The last two run whatever happened above, once everything stopped.
	p.stopDaemon()
	killSessions(p.root)
	waitSessionsGone(p.root)
	t.Run("10 nothing left running, nothing left in the workspace", func(t *testing.T) {
		dirs, _ := filepath.Glob(filepath.Join(p.root, "state", "sessions", "*"))
		for _, d := range dirs {
			for _, f := range []string{host.PIDFile, host.HostPIDFile} {
				if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
					if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); alive(pid) && !zombie(pid) {
						t.Errorf("process %d of %s still runs", pid, filepath.Base(d))
					}
				}
			}
		}
		if th.Thread == "" {
			t.Skip("no thread was opened")
		}
		cmd := exec.Command("git", "status", "--ignored", "--porcelain")
		cmd.Dir = workspace.Path(p.root, remoteSurface, th.Channel, th.Thread)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("git status --ignored in the workspace:\n%s", out)
		}
	})
	t.Run("11 the user's settings file unchanged", func(t *testing.T) {
		if after := userSettings(t); after != settingsBefore {
			t.Fatalf("the user's settings file changed: %s, now %s", settingsBefore, after)
		}
	})
}

// dump logs what the thread did and the end of the daemon's log, for a
// failed step: the instance's directory is removed when the test ends.
func (p *procRoot) dump(t *testing.T, conv surface.Conversation) {
	if conv.Thread == "" {
		return
	}
	rows, err := p.db(t).Query(`SELECT time, kind, data FROM events WHERE thread = ? ORDER BY pos`, conv.Thread)
	if err == nil {
		for rows.Next() {
			var at int64
			var kind string
			var data []byte
			rows.Scan(&at, &kind, &data)
			t.Logf("event %s %s %.300s", time.UnixMilli(at).Format("15:04:05"), kind, data)
		}
		rows.Close()
	}
	// The end of each session's terminal, as text: what the CLI showed.
	logs, _ := filepath.Glob(filepath.Join(p.root, "state", "sessions", "*", host.TerminalLog))
	for _, f := range logs {
		b, _ := os.ReadFile(f)
		text := plainText(b)
		t.Logf("terminal %s, the end:\n%s", f, text[max(0, len(text)-3000):])
	}
	for _, c := range p.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost {
			t.Logf("post %.300q", c.Message.Text)
		}
	}
	p.logMu.Lock()
	l := p.log.String()
	p.logMu.Unlock()
	t.Logf("daemon log, the end:\n%s", l[max(0, len(l)-4000):])
}

// escapes are the terminal's control sequences: CSI, OSC (ended by BEL or
// ST) and the two-byte ones the CLI sends.
var escapes = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[78=>]`)

// plainText is terminal output with its control sequences as spaces.
func plainText(b []byte) []byte { return escapes.ReplaceAll(b, []byte(" ")) }

// realWrapper runs the CLI with the person's own context kept out. The
// adapter adds --settings (its hooks), --permission-mode, the session and
// the model. The wrapper passes a copy of the adapter's settings file with
// the full-screen interface added, which a person's settings may choose;
// nothing in it runs a command.
const realWrapper = `#!/bin/sh
export CLAUDE_CODE_DISABLE_CLAUDE_MDS=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 ENABLE_CLAUDEAI_MCP_SERVERS=false BROWSER=true
n=$# prev=
while [ "$n" -gt 0 ]; do
	a=$1
	shift
	n=$((n - 1))
	if [ "$prev" = --settings ]; then
		{ printf '{"tui":"fullscreen",'; tail -c +2 "$a"; } > "$a.check.json" || exit 1
		a=$a.check.json
	fi
	set -- "$@" "$a"
	prev=$a
done
exec claude --setting-sources project,local "$@"
`

// userSettings is the modification time and checksum of the person's
// Claude Code settings file, or "absent": settings.json in the CLI's
// config directory, ~/.claude unless the environment names another.
func userSettings(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	path := filepath.Join(dir, "settings.json")
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s %x", st.ModTime().Format(time.RFC3339Nano), sha256.Sum256(b))
}

// realEvent is an event as `hataori events` prints it.
type realEvent struct {
	Time time.Time
	Data map[string]any
}

func (p *procRoot) list(t *testing.T, kind string, conv surface.Conversation) []realEvent {
	t.Helper()
	_, out, _ := cli(t, "events", "--root", p.root)
	var got []realEvent
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var e struct {
			Time   time.Time
			Kind   string
			Thread struct{ Scope, Key string }
			Data   map[string]any
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Kind == kind && e.Thread.Key == conv.Thread {
			got = append(got, realEvent{Time: e.Time, Data: e.Data})
		}
	}
	return got
}

func (p *procRoot) turnsEnded(t *testing.T, conv surface.Conversation) []realEvent {
	return p.list(t, "turn_ended", conv)
}

// answer is a post as an answer of the agent: its text, without the case,
// the spaces around it and a final full stop.
func answer(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// replies counts the posts in conv that answer with word.
func (p *procRoot) replies(conv surface.Conversation, word string) int {
	n := 0
	for _, c := range p.driver.Calls(conv) {
		if c.Op == fakesurface.OpPost && answer(c.Message.Text) == word {
			n++
		}
	}
	return n
}

// quiet waits until every turn of conv that started has ended.
func quiet(t *testing.T, p *procRoot, conv surface.Conversation) {
	t.Helper()
	eventually(t, "the thread to be quiet", p.within(3*time.Minute), func() bool {
		return len(p.list(t, "turn_started", conv)) == len(p.turnsEnded(t, conv))
	})
}
