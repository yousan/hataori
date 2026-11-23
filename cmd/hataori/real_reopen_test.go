package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/workspace"
)

// TestRealClaudeCodeReopen checks the reopen (#670) against the real Claude
// Code CLI, as TestRealClaudeCode runs it (the same wrapper, the fake chat
// surface, the model haiku). It runs only when asked for (docs/testing.md):
//
//	HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealClaudeCodeReopen -v -timeout 20m ./cmd/hataori
//
// A thread is told a word and closed; hataori reopen takes it back, and the
// next message resumes the same conversation: the agent still knows the
// word, in the same agent session. Then the thread is removed, its
// workspace goes to trash, and a reopen takes it back from there: the
// conversation resumes again. Removed once more and its entry in trash
// deleted, it is reopened in a new clone, with a new conversation that is
// given the earlier exchange.
func TestRealClaudeCodeReopen(t *testing.T) {
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
	p.deadline = time.Now().Add(15 * time.Minute)
	p.start(t)
	repo := bareRepo(t)

	var (
		th      surface.Conversation
		num     string
		session string
		ws      string
	)
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
	state := func(t *testing.T) string {
		t.Helper()
		_, out, _ := cli(t, "thread", "--root", p.root, num)
		var l struct{ State string }
		json.Unmarshal([]byte(out), &l)
		return l.State
	}
	reopen := func(t *testing.T, became string) {
		t.Helper()
		code, out, errOut := cli(t, "reopen", "--root", p.root, num)
		if code != 0 || !strings.Contains(out, `"workspace":"`+became+`"`) || !strings.Contains(out, `"conversation":"resume"`) {
			t.Fatalf("hataori reopen: %d %q %q", code, out, errOut)
		}
		if s := state(t); s != "asleep" {
			t.Fatalf("state %s after the reopen, want asleep", s)
		}
	}
	recall := func(t *testing.T) {
		t.Helper()
		before := p.replies(th, "heron")
		p.in(th, "Which word did I ask you to remember? Reply with just that word.")
		eventually(t, "the answer heron", p.within(3*time.Minute), func() bool { return p.replies(th, "heron") > before })
		if n, id := p.agentSession(t); n != 1 || id != session || id == "" {
			t.Fatalf("%d sessions, agent session %q then %q; want the same conversation", n, session, id)
		}
	}
	closeIt := func(t *testing.T) {
		t.Helper()
		quiet(t, p, th)
		pid := p.agentPID(t)
		if code, _, errOut := cli(t, "close", "--root", p.root, num); code != 0 {
			t.Fatalf("hataori close: %d %q", code, errOut)
		}
		eventually(t, "the agent to end", p.within(30*time.Second), func() bool { return !alive(pid) || zombie(pid) })
	}

	step("1 a word to remember", func(t *testing.T) {
		p.in(c1, "!go repo:"+repo+" Remember this word for later: heron. Reply with just: remembered")
		th = p.thread(t, 0)
		eventually(t, "the answer remembered", p.within(3*time.Minute), func() bool {
			return len(p.turnsEnded(t, th)) == 1 && p.replies(th, "remembered") > 0
		})
		num = numberOf(t, p.root, th)
		_, session = p.agentSession(t)
		p.db(t).QueryRow(`SELECT workspace FROM threads WHERE id = ?`, num).Scan(&ws)
		if session == "" || ws == "" {
			t.Fatalf("agent session %q, workspace %q", session, ws)
		}
	})
	step("2 closed, reopened on the host, the conversation resumed", func(t *testing.T) {
		closeIt(t)
		reopen(t, "kept")
		recall(t)
	})
	step("3 removed, reopened from trash, the conversation resumed", func(t *testing.T) {
		closeIt(t)
		if code, _, errOut := cli(t, "remove", "--root", p.root, num); code != 0 {
			t.Fatalf("hataori remove: %d %q", code, errOut)
		}
		if _, err := os.Lstat(ws); !os.IsNotExist(err) {
			t.Fatalf("the workspace is in place after the removal: %v", err)
		}
		reopen(t, "restored")
		if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
			t.Fatalf("the workspace is not back: %v", err)
		}
		recall(t)
	})

	step("4 removed and purged, reopened in a new clone, a new conversation", func(t *testing.T) {
		closeIt(t)
		if code, _, errOut := cli(t, "remove", "--root", p.root, num); code != 0 {
			t.Fatalf("hataori remove: %d %q", code, errOut)
		}
		trashed, _ := filepath.Glob(filepath.Join(p.root, workspace.TrashDir, "*"))
		if len(trashed) != 1 {
			t.Fatalf("trash holds %v", trashed)
		}
		if err := os.RemoveAll(trashed[0]); err != nil { // as the trash pass does
			t.Fatal(err)
		}
		code, out, errOut := cli(t, "reopen", "--root", p.root, num)
		if code != 0 || !strings.Contains(out, `"workspace":"remade"`) || !strings.Contains(out, `"conversation":"new"`) {
			t.Fatalf("hataori reopen: %d %q %q", code, out, errOut)
		}
		// The word is in the earlier exchange its first prompt gives it.
		before := p.replies(th, "heron")
		p.in(th, "Which word did I ask you to remember? Reply with just that word.")
		eventually(t, "the answer heron", p.within(3*time.Minute), func() bool { return p.replies(th, "heron") > before })
		if _, id := p.agentSession(t); id == "" || id == session {
			t.Fatalf("agent session %q, before %q; want a new conversation", id, session)
		}
	})

	p.stopDaemon()
	killSessions(p.root)
	waitSessionsGone(p.root)
	t.Run("5 nothing left running", func(t *testing.T) {
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
	})
	t.Run("6 the user's settings file unchanged", func(t *testing.T) {
		if after := userSettings(t); after != settingsBefore {
			t.Fatalf("the user's settings file changed: %s, now %s", settingsBefore, after)
		}
	})
}
