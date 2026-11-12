package claudecode

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	if b, err := a.LoginCode(" AbC-123_x#y \n"); err != nil || string(b) != "AbC-123_x#y\n" {
		t.Fatalf("a code: %q, %v", b, err)
	}
	for _, bad := range []string{"", "  ", "two words", "tab\there", "line\nbreak", "esc\x1b[0m"} {
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

// The login functions keep nothing: login.go reaches no file, process or
// network, and LoginLaunch asks for no file.
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
			t.Fatalf("timed out; so far %+v", got)
		}
	}
	if !strings.HasPrefix(got.Link, "https://") || !got.WantsCode {
		t.Fatalf("real login: %+v", got)
	}
	t.Logf("link read (%d characters), code requested, wrong code refused: %q", len(got.Link), got.Refused)
	if _, err := os.Stat(filepath.Join(cfg, ".credentials.json")); err == nil {
		t.Fatal("a login was stored in the throwaway directory")
	}
}
