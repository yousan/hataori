package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func configDir(env []string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			return v, true
		}
	}
	return "", false
}

// The login command and the status check of a session that was taken in run
// with its configuration directory.
func TestLoginUsesTheConfigDirOfASessionTakenIn(t *testing.T) {
	a := newAdapter(t, "")
	a.environ = func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/user", "CLAUDE_CONFIG_DIR=/srv/claude-config"}
	}
	s := spec(t)
	s.ConfigDir = "/home/user/.claude-work"
	session, _ := a.Launch(s)
	login, err := a.LoginLaunch(s)
	if err != nil {
		t.Fatal(err)
	}
	sd, _ := configDir(session.Env)
	ld, _ := configDir(login.Env)
	if sd != s.ConfigDir {
		t.Fatalf("the session's config dir is %q, want %q", sd, s.ConfigDir)
	}
	if ld != sd {
		t.Fatalf("the session that is logged out uses %q, but the login command logs in %q", sd, ld)
	}

	// A stand-in CLI that is logged in in one directory only.
	cli := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
case "$1" in
--version) echo "2.1.331 (Claude Code)" ;;
auth) if [ "$CLAUDE_CONFIG_DIR" = /home/user/.claude-work ]; then echo '{"loggedIn": true}'; else echo '{"loggedIn": false}'; exit 1; fi ;;
esac
`
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a.command = cli
	for dir, want := range map[string]bool{"": false, s.ConfigDir: true} {
		h, err := a.Probe(context.Background(), agent.Spec{ConfigDir: dir})
		if err != nil || h.LoggedIn != want {
			t.Errorf("logged in with the directory %q: %v (%v), want %v", dir, h.LoggedIn, err, want)
		}
	}
}

// The directory that the daemon's sessions use anyway is no login of its own.
func TestLoginDir(t *testing.T) {
	const home = "/home/user"
	byDefault := "" // of a CLAUDE_CONFIG_DIR that is not set
	if runtime.GOOS == "darwin" {
		byDefault = home + "/.claude"
	}
	for _, c := range []struct{ own, dir, want string }{
		{"", "", ""},
		{"/srv/claude-config", "", ""},
		{"/srv/claude-config", "/srv/claude-config", ""},
		{"/srv/claude-config", "/srv/claude-config/", ""},
		{"/srv/claude-config", home + "/.claude", home + "/.claude"},
		{"", home + "/.claude", byDefault},
		{"", home + "/.claude-work", home + "/.claude-work"},
		{"", home + "/.claude-work/", home + "/.claude-work"},
	} {
		a := newAdapter(t, "")
		a.environ = func() []string {
			env := []string{"PATH=/usr/bin", "HOME=" + home}
			if c.own != "" {
				env = append(env, "CLAUDE_CONFIG_DIR="+c.own)
			}
			return env
		}
		if got := agent.LoginDir(a, agent.Spec{ConfigDir: c.dir}); got != c.want {
			t.Errorf("CLAUDE_CONFIG_DIR %q, session's %q: login of %q, want %q", c.own, c.dir, got, c.want)
		}
	}
}

func TestLoginLaunchUsesTheSessionsConfigDir(t *testing.T) {
	for _, environ := range [][]string{
		{"PATH=/usr/bin", "HOME=/home/user", "CLAUDE_CONFIG_DIR=/srv/claude-config"},
		{"PATH=/usr/bin", "HOME=/home/user"},
	} {
		a := newAdapter(t, "")
		a.environ = func() []string { return environ }
		s := spec(t)
		session, _ := a.Launch(s)
		login, err := a.LoginLaunch(s)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(login.Argv, []string{"claude", "auth", "login"}) || login.Terminal || len(login.Files) != 0 {
			t.Fatalf("login launch: %+v", login)
		}
		sd, sok := configDir(session.Env)
		ld, lok := configDir(login.Env)
		if sd != ld || sok != lok {
			t.Fatalf("config dir: session %q (%v), login %q (%v)", sd, sok, ld, lok)
		}
		if !slices.Contains(login.Env, "BROWSER=true") {
			t.Fatal("the login command may open a browser on the host")
		}
	}
}

// decodeAll splits captured output into lines, the last one possibly not
// ended, and decodes each.
func decodeAll(a *Adapter, out []byte) (st agent.LoginStep) {
	for _, l := range bytes.Split(out, []byte("\n")) {
		s := a.LoginDecode(l)
		if s.Link != "" {
			st.Link = s.Link
		}
		st.WantsCode = st.WantsCode || s.WantsCode
		if s.Refused != "" {
			st.Refused = s.Refused
		}
	}
	return st
}

func TestLoginDecodeCaptures(t *testing.T) {
	a := newAdapter(t, "")
	pipes := readCapture(t, "login/stdout.txt")
	want := strings.TrimSpace(string(pipes[bytes.Index(pipes, []byte("https://")):bytes.Index(pipes, []byte("\nPaste"))]))
	refusal := strings.TrimSpace(string(readCapture(t, "login/stderr.txt")))

	got := decodeAll(a, readCapture(t, "login/stdout-before-code.txt"))
	if got.Link != want || !got.WantsCode || got.Refused != "" {
		t.Fatalf("pipes, before the code: %+v", got)
	}
	if got := decodeAll(a, readCapture(t, "login/stderr.txt")); got.Refused != refusal || refusal == "" {
		t.Fatalf("pipes, wrong code: %+v", got)
	}
	// On a terminal the link is inside a hyperlink sequence and coloured.
	got = decodeAll(a, readCapture(t, "login_terminal/terminal.raw"))
	if got.Link != want || !got.WantsCode || got.Refused != refusal {
		t.Fatalf("terminal: %+v\nwant link %q", got, want)
	}
	if !strings.HasPrefix(want, "https://claude.com/") || !strings.HasSuffix(want, "state=REDACTED") {
		t.Fatalf("the captured link is not whole: %q", want)
	}
}

func TestLoginCode(t *testing.T) {
	a := newAdapter(t, "")
	if b, err := a.LoginCode("AbC-123_x#y"); err != nil || string(b) != "AbC-123_x#y\n" {
		t.Fatalf("a code: %q, %v", b, err)
	}
	for _, bad := range []string{"", "  ", " code", "code ", "code\n", "two words", "tab\there", "esc\x1b[0m"} {
		if _, err := a.LoginCode(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseStatusCaptures(t *testing.T) {
	for file, want := range map[string]bool{
		"login/status-before.json":         false,
		"probe/auth-status-logged-in.json": true,
	} {
		got, err := ParseStatus(readCapture(t, file))
		if err != nil || got != want {
			t.Errorf("%s: %v, %v", file, got, err)
		}
	}
	if _, err := ParseStatus([]byte(`{"other":1}`)); err == nil {
		t.Error("a status without loggedIn was read")
	}
}

// The login functions keep nothing. They run in a child process that may
// not write any file (RLIMIT_FSIZE 0, in a directory that is gone, with no
// HOME); every one of them still gives its result.
func TestLoginRunsWithoutAFilesystem(t *testing.T) {
	cmd := exec.Command(os.Args[0], "role", "login-pure")
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "ok\n" {
		t.Fatalf("child: %q, %v", out, err)
	}
}

// loginPure is the child of TestLoginRunsWithoutAFilesystem.
func loginPure() int {
	dir, _ := os.MkdirTemp("", "pure-")
	os.Chdir(dir)
	os.Remove(dir)
	if syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: 0}) != nil {
		return 2
	}
	signal.Ignore(syscall.SIGXFSZ) // a write then fails instead of killing the process
	if f, err := os.Create("/tmp/pure-probe"); err == nil {
		_, werr := f.Write([]byte("x"))
		f.Close()
		os.Remove("/tmp/pure-probe")
		if werr == nil {
			return 3 // the limit does not hold; the test would prove nothing
		}
	}
	a := &Adapter{command: "claude", environ: func() []string { return []string{"CLAUDE_CONFIG_DIR=/nowhere"} }}
	l, err := a.LoginLaunch(agent.Spec{})
	st := a.LoginDecode([]byte("If the browser didn't open, visit: https://example.com/x?state=s"))
	code, cerr := a.LoginCode("abc")
	in, perr := ParseStatus([]byte(`{"loggedIn":true}`))
	if err != nil || len(l.Argv) != 3 || st.Link == "" || cerr != nil || string(code) != "abc\n" || perr != nil || !in {
		return 4
	}
	os.Stdout.WriteString("ok\n")
	return 0
}

// login.go also imports nothing that reaches a file, a process or the
// network.
func TestLoginIsPure(t *testing.T) {
	src, err := os.ReadFile("login.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{`"os"`, `"os/exec"`, `"net"`, `"io"`, `"log"`, `"log/slog"`, `"syscall"`} {
		if bytes.Contains(src, []byte(pkg)) {
			t.Errorf("login.go imports %s", pkg)
		}
	}
}

// TestRealLogin runs the real `claude auth login`, only with a throwaway
// config directory and only a wrong code. It is skipped unless
// HATAORI_REAL_CLAUDE=1; see docs/testing.md.
func TestRealLogin(t *testing.T) {
	if os.Getenv("HATAORI_REAL_CLAUDE") != "1" {
		t.Skip("set HATAORI_REAL_CLAUDE=1 to run against the installed CLI")
	}
	cfg := t.TempDir()
	a := newAdapter(t, "")
	a.environ = func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CLAUDE_CONFIG_DIR=" + cfg}
	}
	l, _ := a.LoginLaunch(agent.Spec{})
	if d, _ := configDir(l.Env); d != cfg || !strings.HasPrefix(d, os.TempDir()) {
		t.Fatalf("refusing to run: config dir %q is not the throwaway one", d)
	}
	cmd := exec.Command(l.Argv[0], l.Argv[1:]...)
	cmd.Env, cmd.Dir = l.Env, cfg
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	steps := make(chan agent.LoginStep, 16)
	read := func(r io.Reader) {
		br := bufio.NewReader(r)
		var pending []byte
		for {
			b := make([]byte, 4096)
			n, err := br.Read(b)
			pending = append(pending, b[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				steps <- a.LoginDecode(pending[:i])
				pending = pending[i+1:]
			}
			if len(pending) > 0 {
				steps <- a.LoginDecode(pending) // the prompt for a code has no newline
			}
			if err != nil {
				return
			}
		}
	}
	go read(stdout)
	go read(stderr)
	var got agent.LoginStep
	deadline := time.After(60 * time.Second)
	for got.Refused == "" {
		select {
		case s := <-steps:
			if s.Link != "" {
				got.Link = s.Link
			}
			if s.WantsCode && !got.WantsCode {
				got.WantsCode = true
				code, _ := a.LoginCode("not-a-valid-code")
				stdin.Write(code)
			}
			if s.Refused != "" {
				got.Refused = s.Refused
			}
		case <-deadline:
			// Never print the link itself.
			t.Fatalf("timed out; link read: %v, code requested: %v", got.Link != "", got.WantsCode)
		}
	}
	if !strings.HasPrefix(got.Link, "https://") || !got.WantsCode {
		t.Fatalf("real login: link read %v, code requested %v", strings.HasPrefix(got.Link, "https://"), got.WantsCode)
	}
	t.Logf("link read (%d characters), code requested, wrong code refused: %q", len(got.Link), got.Refused)
	if _, err := os.Stat(filepath.Join(cfg, ".credentials.json")); err == nil {
		t.Fatal("a login was stored in the throwaway directory")
	}
}
