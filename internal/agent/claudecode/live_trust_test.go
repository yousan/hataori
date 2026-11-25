package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
)

// TestLiveTrustFiles starts the real CLI as hataori launches it, in a new
// repository that holds a project settings file, and lets the codec deal
// with the folder-trust dialog that names the file (#423). It runs only
// when asked for:
//
//	HATAORI_REAL_CLAUDE=1 HATAORI_CAPTURE_DIR=<raw dir> go test -run TestLiveTrustFiles -v ./internal/agent/claudecode
//
// Each case runs with a new, empty CLAUDE_CONFIG_DIR, so nothing of the
// user's settings or login is read or written and no login is needed: the
// test writes only there, a .claude.json that skips the first-run screens
// and, for the notice (#707), a settings.json, and removes it. The
// hooks are a recorder given with --settings in place of hataori's. The raw
// dir gets, per case, the screen as the codec was given it up to its key
// or its event, the keys, the events and the hook calls: run scrub.py on
// it before anything is committed.
func TestLiveTrustFiles(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run the real CLI")
	}
	raw := os.Getenv("HATAORI_CAPTURE_DIR")
	if raw == "" {
		t.Fatal("set HATAORI_CAPTURE_DIR")
	}
	settings := `{"permissions": {"allow": []}}` + "\n"
	// A status line, a hook and environment: what a supervisor could plant.
	local := `{"statusLine": {"type": "command", "command": "true"}, "env": {"X": "1"},` +
		` "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "true"}]}]}}` + "\n"
	// Hooks in the project settings, and tools they allow: the warnings
	// 2.1.329 draws above the line naming the files (#592, #596).
	hooks := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "true"}]}]}}` + "\n"
	permissions := `{"permissions": {"allow": ["Bash(ls:*)"]}}` + "\n"
	many := `{"permissions": {"allow": ["Bash(ls:*)", "Trusting this folder loads x.", "Read(/etc/**)", "Bash(a, b and c)",` +
		` "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(cat:*)"]}}` + "\n"
	dirs := `{"permissions": {"allow": ["Bash(ls:*)"], "additionalDirectories": ["/etc"]}}` + "\n"
	for _, tc := range []struct {
		name     string
		files    map[string]string
		spec     agent.Spec
		answered bool
		ignored  bool // a screen the codec does not know: no key and no event
	}{
		// The notice above the dialog (#707): see interruptedStart.
		{"trust_notice_allowed", nil, agent.Spec{}, true, false},
		{"trust_notice_refused", map[string]string{".claude/settings.json": settings}, agent.Spec{}, false, false},
		// The other one, after a second such start (#728).
		{"trust_notice_off_allowed", nil, agent.Spec{}, true, false},
		{"trust_notice_off_refused", map[string]string{".claude/settings.json": settings}, agent.Spec{}, false, false},
		{"trust_hooks_refused", map[string]string{".claude/settings.json": hooks}, agent.Spec{}, false, false},
		{"trust_hooks_allowed", map[string]string{".claude/settings.json": hooks},
			agent.Spec{TrustFiles: []string{".claude/settings.json"}}, true, false},
		{"trust_permissions_refused", map[string]string{".claude/settings.json": permissions}, agent.Spec{}, false, false},
		{"trust_permissions_allowed", map[string]string{".claude/settings.json": permissions},
			agent.Spec{TrustFiles: []string{".claude/settings.json"}}, true, false},
		// Two files, more rules than the dialog shows, and one that writes
		// the dialog's own line.
		{"trust_permissions_both", map[string]string{".claude/settings.json": many, ".claude/settings.local.json": permissions},
			agent.Spec{TrustFiles: []string{".claude/settings.json", ".claude/settings.local.json"}}, true, false},
		// Another warning below the permissions: not known.
		{"trust_permissions_dirs", map[string]string{".claude/settings.json": dirs},
			agent.Spec{TrustFiles: []string{".claude/settings.json"}}, false, true},
		{"trust_files_refused", map[string]string{".claude/settings.json": settings}, agent.Spec{}, false, false},
		{"trust_files_allowed", map[string]string{".claude/settings.json": settings},
			agent.Spec{TrustFiles: []string{".claude/settings.json"}}, true, false},
		{"trust_files_confined", map[string]string{".claude/settings.local.json": local},
			agent.Spec{Confine: &agent.Confinement{}}, true, false},
		// The other wordings: two files, and an MCP config.
		{"trust_files_both", map[string]string{".claude/settings.json": settings, ".claude/settings.local.json": local},
			agent.Spec{TrustFiles: []string{".claude/settings.json", ".claude/settings.local.json"}}, true, false},
		// A note on the only file (#730).
		{"trust_files_env", map[string]string{".claude/settings.json": `{"env": {"X": "1"}}` + "\n"},
			agent.Spec{TrustFiles: []string{".claude/settings.json"}}, true, false},
		{"trust_files_mcp", map[string]string{".mcp.json": `{"mcpServers": {"x": {"type": "stdio", "command": "true"}}}` + "\n"},
			agent.Spec{}, false, false},
	} {
		if only := os.Getenv("HATAORI_CAPTURE_ONLY"); only != "" && !strings.HasPrefix(tc.name, only) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) { liveTrust(t, raw, tc.name, tc.files, tc.spec, tc.answered, tc.ignored) })
	}
}

func liveTrust(t *testing.T, raw, name string, files map[string]string, spec agent.Spec, answered, ignored bool) {
	out := filepath.Join(raw, name)
	hooks := filepath.Join(out, "hooks")
	if _, err := os.Stat(out); err == nil {
		t.Fatalf("%s exists: give a new HATAORI_CAPTURE_DIR", out)
	}
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	work := liveTempDir(t)
	defer os.RemoveAll(work)
	cfg := filepath.Join(work, "config")
	dir := filepath.Join(work, "sessions", name)
	state := filepath.Join(work, "state")
	for _, d := range []string{cfg, dir, state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(`{"hasCompletedOnboarding": true, "theme": "dark"}`), 0o600)
	git := exec.Command("git", "init", "-q", dir)
	git.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work, "GIT_CONFIG_NOSYSTEM=1"}
	if b, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, b)
	}
	for p, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700)
		os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600)
	}

	notice := ""
	if strings.HasPrefix(name, "trust_notice") {
		notice = fullscreenNotices[0]
		interruptedStart(t, work, cfg, "fullscreenBootPending")
	}
	if strings.HasPrefix(name, "trust_notice_off") {
		notice = fullscreenNotices[1]
		// The next start counts the first and uses the other interface; the
		// one after it tries again, and is interrupted too.
		interruptedStart(t, work, cfg, "fullscreenBootStrikes")
		interruptedStart(t, work, cfg, "fullscreenBootPending")
	}

	// hataori's launch, with the recorder's settings in place of its own.
	spec.Dir, spec.StateDir, spec.Model = dir, state, "haiku"
	// No update: it would change the installed CLI, and a new version counts
	// the interrupted starts anew.
	env := []string{"CLAUDE_CONFIG_DIR=" + cfg, "DISABLE_AUTOUPDATER=1"}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "SHELL", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	a := &Adapter{command: "claude", hataori: "hataori", hookTimeout: time.Hour, environ: func() []string { return env }}
	l, err := a.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	for f, b := range l.Files {
		os.WriteFile(filepath.Join(state, f), b, 0o600)
	}
	recorder := filepath.Join(work, "recorder.json")
	hook := map[string]any{"type": "command", "command": `f="$CAPTURE_HOOK_DIR/$(date +%s%N)"; cat > "$f.tmp"; ` +
		`ev=$(sed -n 's/.*"hook_event_name":"\([A-Za-z]*\)".*/\1/p' "$f.tmp"); mv "$f.tmp" "$f-$ev.json"`}
	hs := map[string]any{}
	for _, ev := range []string{"SessionStart", "SessionEnd"} {
		hs[ev] = []any{map[string]any{"hooks": []any{hook}}}
	}
	sb, _ := json.Marshal(map[string]any{"hooks": hs, "remoteControlAtStartup": false})
	os.WriteFile(recorder, sb, 0o600)
	argv := append([]string{}, l.Argv...)
	for i := range argv {
		if i > 0 && argv[i-1] == "--settings" {
			argv[i] = recorder
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, l.Env...), "BROWSER=true", "CAPTURE_HOOK_DIR="+hooks)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80}) // the session host's default size
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() {
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Process.Kill()
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
			if strings.HasSuffix(n.Name(), "-SessionStart.json") {
				return true
			}
		}
		return false
	}

	c := a.NewCodec(spec).(*codec)
	c.Open()
	var keys, events []string
	var drawn []byte
	n, done := 0, false
	act := func(x Actions, after string) {
		for _, e := range x.Events {
			events = append(events, fmt.Sprintf("after %s: %s %q %q", after, describe(e), e.Reason, e.Text))
			if e.Kind == agent.SessionEnded {
				done = true
			}
		}
		for _, k := range x.Keys {
			w, err := holder.Encode(k)
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, fmt.Sprintf("after %s: %q", after, w))
			if k == keyEnter {
				done = true
			}
			if _, err := tty.Write(w); err != nil {
				t.Fatal(err)
			}
		}
	}
	deadline := time.After(60 * time.Second)
	if ignored {
		// The screen settles, the codec looks at it for a while, and stays
		// silent.
		deadline = time.After(15 * time.Second)
	}
	last := time.Now()
	for !done {
		select {
		case b, ok := <-chunks:
			if !ok {
				t.Fatalf("the CLI ended; keys %q events %q", keys, events)
			}
			n++
			drawn = append(drawn, b...)
			os.WriteFile(filepath.Join(out, fmt.Sprintf("screen-%02d.raw", n)), b, 0o600)
			act(c.Decode(agent.Screen, b), fmt.Sprintf("screen-%02d", n))
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			if ignored {
				done = true
				continue
			}
			t.Fatalf("neither a key nor an event within 60 s; keys %q events %q", keys, events)
		}
		if time.Since(last) >= time.Second {
			last = time.Now()
			act(c.Tick(last), fmt.Sprintf("screen-%02d and a tick", n))
		}
	}
	os.WriteFile(filepath.Join(out, "keys.txt"), []byte(strings.Join(keys, "\n")+"\n"), 0o600)
	os.WriteFile(filepath.Join(out, "events.txt"), []byte(strings.Join(events, "\n")+"\n"), 0o600)
	if !strings.HasPrefix(string(drawn), notice) {
		t.Fatal("the CLI did not print the notice: the capture is not of it")
	}
	if ignored {
		if len(keys) != 0 || len(events) != 0 {
			t.Fatalf("keys %q events %q, want none for a screen the codec does not know", keys, events)
		}
		return
	}
	if !answered {
		if len(keys) != 0 || len(events) != 1 {
			t.Fatalf("keys %q events %q, want no key and one event", keys, events)
		}
		time.Sleep(3 * time.Second)
		if started() {
			t.Fatal("SessionStart came for a dialog nobody answered")
		}
		return
	}
	// Answered: the CLI starts.
	for wait := time.After(30 * time.Second); !started(); {
		select {
		case <-chunks:
		case <-time.After(200 * time.Millisecond):
		case <-wait:
			t.Fatalf("no SessionStart within 30 s of the answer; keys %q events %q", keys, events)
		}
	}
	t.Logf("SessionStart after keys %q", keys)
	for _, r := range "/exit" {
		tty.Write([]byte(string(r)))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	tty.Write([]byte("\r"))
	for wait := time.After(20 * time.Second); ; {
		select {
		case _, ok := <-chunks:
			if !ok {
				return
			}
		case <-wait:
			t.Log("the CLI did not end after /exit; killed")
			return
		}
	}
}

// interruptedStart starts the CLI in a directory of its own with
// "tui": "fullscreen" in cfg's settings, answers the trust dialog if it
// comes, and kills the CLI as soon as its state holds until. Killed at
// "fullscreenBootPending", which names the start from the moment its
// full-screen interface began, it leaves a start that did not finish, and
// the next one prints so (#707). That next one counts it, and is killed at
// "fullscreenBootStrikes".
func interruptedStart(t *testing.T, work, cfg, until string) {
	dir := filepath.Join(work, "killed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"tui": "fullscreen"}`+"\n"), 0o600)
	cmd := exec.Command("claude", "--model", "haiku")
	cmd.Dir = dir
	cmd.Env = []string{"CLAUDE_CONFIG_DIR=" + cfg, "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TERM=xterm-256color", "BROWSER=true", "DISABLE_AUTOUPDATER=1"}
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	defer cmd.Wait()
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	var mu sync.Mutex
	var seen []byte
	go func() {
		b := make([]byte, 64<<10)
		for {
			n, err := tty.Read(b)
			mu.Lock()
			seen = append(seen, b[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	yes := false
	for i, quiet, was := 0, 0, 0; i < 3000; i++ {
		time.Sleep(10 * time.Millisecond)
		if b, _ := os.ReadFile(filepath.Join(cfg, ".claude.json")); strings.Contains(string(b), until) {
			return
		}
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n != was {
			quiet, was = 0, n
		} else if quiet++; n > 0 && quiet == 200 && !yes { // the dialog has settled
			yes = true
			tty.Write([]byte("\x1b[B"))
			time.Sleep(time.Second)
			tty.Write([]byte("\r"))
		}
	}
	t.Fatalf("the CLI's state never held %s", until)
}
