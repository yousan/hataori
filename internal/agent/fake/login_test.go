package fake

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

func loginAdapter(t *testing.T, l Login) *Adapter {
	t.Helper()
	cfg, _ := json.Marshal(Config{Command: []string{os.Args[0], "role", "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")},
		ConfigDir: t.TempDir(), Login: l})
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Adapter)
}

// login runs the adapter's login command on pipes, as the relay will.
type login struct {
	t     *testing.T
	a     *Adapter
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
}

func startLogin(t *testing.T, a *Adapter) *login {
	t.Helper()
	l, err := a.LoginLaunch(agent.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(l.Argv[0], l.Argv[1:]...)
	cmd.Env = l.Env
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return &login{t: t, a: a, cmd: cmd, in: in, lines: lines}
}

// step reads the command's next line.
func (l *login) step() agent.LoginStep {
	l.t.Helper()
	select {
	case line, ok := <-l.lines:
		if !ok {
			l.t.Fatal("the login command ended")
		}
		return l.a.LoginDecode([]byte(line))
	case <-time.After(10 * time.Second):
		l.t.Fatal("no line from the login command")
	}
	return agent.LoginStep{}
}

func (l *login) code(c string) {
	l.t.Helper()
	b, err := l.a.LoginCode(c)
	if err != nil {
		l.t.Fatal(err)
	}
	l.in.Write(b)
}

func (l *login) exit() int {
	l.in.Close()
	l.cmd.Wait()
	return l.cmd.ProcessState.ExitCode()
}

func loggedInNow(t *testing.T, a *Adapter) bool {
	h, err := a.Probe(context.Background(), agent.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	return h.LoggedIn
}

func TestLoginAcceptsOnlyItsCode(t *testing.T) {
	a := loginAdapter(t, Login{Code: "good-code"})
	if loggedInNow(t, a) {
		t.Fatal("logged in before any login")
	}
	l := startLogin(t, a)
	link := l.step().Link
	if link == "" || !l.step().WantsCode {
		t.Fatal("no link and request for a code")
	}
	l.code("wrong")
	if s := l.step(); s.Refused == "" {
		t.Fatalf("a wrong code: %+v", s)
	}
	if !l.step().WantsCode {
		t.Fatal("it did not ask again")
	}
	if loggedInNow(t, a) {
		t.Fatal("logged in after a wrong code")
	}
	l.code("good-code")
	l.step() // "Logged in."
	if code := l.exit(); code != 0 || !loggedInNow(t, a) {
		t.Fatalf("exit %d, logged in %v", code, loggedInNow(t, a))
	}
	// Each start gives a new link.
	l2 := startLogin(t, a)
	if again := l2.step().Link; again == "" || again == link {
		t.Fatalf("links %q and %q", link, again)
	}
}

func TestLoginWithoutLinkOrWithoutLoggingIn(t *testing.T) {
	a := loginAdapter(t, Login{Code: "c", NoLink: true})
	l := startLogin(t, a)
	select {
	case line := <-l.lines:
		t.Fatalf("printed %q", line)
	case <-time.After(300 * time.Millisecond):
	}
	l.exit()

	a = loginAdapter(t, Login{Code: "c", Quit: true})
	l = startLogin(t, a)
	l.step()
	l.step()
	l.code("c")
	if code := l.exit(); code != 0 || loggedInNow(t, a) {
		t.Fatalf("exit %d, logged in %v: want a clean end without a login", code, loggedInNow(t, a))
	}
}

// Turns fail as not logged in until the login has succeeded.
func TestTurnsFailUntilLoggedIn(t *testing.T) {
	a := loginAdapter(t, Login{Code: "good-code"})
	s := start(t, a, agent.Spec{})
	s.until("ready", s.has(agent.SessionReady))
	s.send("reply hello")
	s.until("the end", s.has(agent.TurnEnded))
	if e := s.events[len(s.events)-1]; e.Status != agent.Failed || e.Reason != agent.Login {
		t.Fatalf("turn before the login: %s", describe(s.events))
	}
	l := startLogin(t, a)
	l.step()
	l.step()
	l.code("good-code")
	l.step()
	l.exit()
	s.send("reply hello")
	s.until("the second end", func() bool { return s.count(agent.TurnEnded) == 2 })
	if e := s.events[len(s.events)-1]; e.Status != agent.Completed {
		t.Fatalf("turn after the login: %s", describe(s.events))
	}
}

func TestLoginCodeIsOneWord(t *testing.T) {
	a := loginAdapter(t, Login{})
	for _, c := range []string{"", "two words", "line\n"} {
		if _, err := a.LoginCode(c); err == nil {
			t.Errorf("code %q accepted", c)
		}
	}
	if _, err := os.Stat(filepath.Join(a.c.ConfigDir, LoggedInFile)); err == nil {
		t.Fatal("logged in")
	}
}

func TestRelativeConfigDirIsRefused(t *testing.T) {
	cfg, _ := json.Marshal(Config{Command: []string{"x"}, ConfigDir: "relative/dir"})
	if _, err := New(cfg); err == nil {
		t.Fatal("a relative config directory was accepted")
	}
}
