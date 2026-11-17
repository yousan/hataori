package claudecode

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/yousan/hataori/internal/agent"
)

// TestLiveFolderTrust starts the real CLI in a clone never opened before
// and lets the codec answer the folder-trust dialog, until SessionStart.
// It runs only when asked for:
//
//	HATAORI_REAL_CLAUDE=1 HATAORI_CAPTURE_DIR=<raw dir> go test -run TestLive -v ./internal/agent/claudecode
//
// It keeps the user's context out as capture.py does (README): only
// --settings and the project's own settings, no CLAUDE.md, no memory, no
// connectors, the user's plugins switched off, the cheapest model, no
// prompt. It writes no settings file of the user's; the CLI itself records
// that it trusts the new directory. The raw dir gets the screen as the
// codec was given it, up to the key that confirmed the dialog, the keys,
// and the hook calls: run scrub.py on it before anything is committed.
func TestLiveFolderTrust(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run the real CLI")
	}
	raw := os.Getenv("HATAORI_CAPTURE_DIR")
	if raw == "" {
		t.Fatal("set HATAORI_CAPTURE_DIR")
	}
	out := filepath.Join(raw, "trust_answered")
	hooks := filepath.Join(out, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	work := liveTempDir(t)
	defer os.RemoveAll(work)

	// A clone never opened on this machine: a new path every run.
	src, clone := filepath.Join(work, "src"), filepath.Join(work, "sessions", "trust_answered")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	os.MkdirAll(src, 0o700)
	git(src, "init", "-q")
	os.WriteFile(filepath.Join(src, "README"), []byte("hello\n"), 0o600)
	git(src, "add", "README")
	git(src, "commit", "-q", "-m", "first")
	os.MkdirAll(filepath.Dir(clone), 0o700)
	git(work, "clone", "-q", src, clone)

	settings := filepath.Join(work, "settings.json")
	hook := map[string]any{"type": "command", "command": `cat > "$CAPTURE_HOOK_DIR/$(date +%s%N).json"`}
	hs := map[string]any{}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		hs[ev] = []any{map[string]any{"hooks": []any{hook}}}
	}
	off := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")); err == nil {
		var u struct {
			EnabledPlugins map[string]any `json:"enabledPlugins"`
		}
		json.Unmarshal(b, &u)
		for k := range u.EnabledPlugins {
			off[k] = false
		}
	}
	sb, _ := json.Marshal(map[string]any{"hooks": hs, "remoteControlAtStartup": false, "enabledPlugins": off})
	os.WriteFile(settings, sb, 0o600)

	cmd := exec.Command("claude", "--settings", settings, "--setting-sources", "project,local", "--model", "haiku",
		"--permission-mode", "default", "--session-id", newUUID())
	cmd.Dir = clone
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "SHELL", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "BROWSER=true", "CAPTURE_HOOK_DIR="+hooks,
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80}) // the session host's default size
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() {
		syscall.Kill(-pid, syscall.SIGKILL) // its own process group, if anything is left
		cmd.Wait()
		tty.Close()
	}()

	chunks := make(chan []byte, 64)
	go func() {
		for {
			b := make([]byte, 64<<10)
			n, err := tty.Read(b)
			if n > 0 {
				chunks <- b[:n]
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()
	started := func() bool {
		names, _ := os.ReadDir(hooks)
		for _, n := range names {
			if b, _ := os.ReadFile(filepath.Join(hooks, n.Name())); bytes.Contains(b, []byte(`"SessionStart"`)) {
				return true
			}
		}
		return false
	}

	c := (&Adapter{}).NewCodec(agent.Spec{Dir: clone})
	if !c.Open().WatchScreen {
		t.Fatal("the codec does not ask for the screen")
	}
	var keys []string
	n, confirmed := 0, false
	send := func(a Actions, after string) {
		for _, w := range a.Write {
			keys = append(keys, fmt.Sprintf("after %s: %q", after, w))
			if bytes.Equal(w, keyEnter) {
				confirmed = true
			}
			if _, err := tty.Write(w); err != nil {
				t.Fatal(err)
			}
		}
	}
	deadline := time.After(60 * time.Second)
	lastTick := time.Now()
	for !started() {
		select {
		case b, ok := <-chunks:
			if !ok {
				t.Fatalf("the CLI ended before SessionStart; keys %q", keys)
			}
			n++
			if confirmed {
				// For looking at by hand only: not scrubbed, never committed.
				os.WriteFile(filepath.Join(raw, fmt.Sprintf("after-%02d.raw", n)), b, 0o600)
				continue
			}
			os.WriteFile(filepath.Join(out, fmt.Sprintf("screen-%02d.raw", n)), b, 0o600)
			send(c.Decode(agent.Screen, b), fmt.Sprintf("screen-%02d", n))
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no SessionStart within 60 s; keys %q", keys)
		}
		if time.Since(lastTick) >= time.Second { // as the runner ticks
			lastTick = time.Now()
			send(c.Tick(lastTick), fmt.Sprintf("screen-%02d and a tick", n))
		}
	}
	os.WriteFile(filepath.Join(out, "keys.txt"), []byte(strings.Join(keys, "\n")+"\n"), 0o600)
	t.Logf("SessionStart after keys %q", keys)

	// Out again: /exit, typed.
	for _, r := range "/exit" {
		tty.Write([]byte(string(r)))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	tty.Write(keyEnter)
	done := make(chan struct{})
	go func() {
		for range chunks {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Log("the CLI did not end after /exit; killed")
	}
}

// liveTempDir makes a new directory capture-XXXXXXXX in the temporary
// directory, the name scrub.py replaces.
func liveTempDir(t *testing.T) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	d := filepath.Join(os.TempDir(), "capture-"+string(b))
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}
