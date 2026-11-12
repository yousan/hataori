package fake

import (
	"bufio"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// Login is what the fake's login command does. It is part of Config.
type Login struct {
	// Code is the code it accepts; any other is refused, and it asks again.
	Code string `json:"code"`
	// NoLink makes it never print a link; it waits for input and exits
	// when its input ends.
	NoLink bool `json:"no_link"`
	// Quit makes it end cleanly at the first code, without logging in.
	Quit bool `json:"quit"`
	// Linger makes it keep running when its input ends, as a command that
	// does not watch its input would, until it is killed.
	Linger bool `json:"linger"`
}

// LoggedInFile marks the fake as logged in, in its config directory.
const LoggedInFile = "logged-in"

// The lines the login command prints.
const (
	loginLinkPrefix = "Open this link to sign in: "
	loginPrompt     = "Enter the code from the page:"
	loginRefused    = "That code is not valid. Try again."
	loginDone       = "Logged in."
)

var _ agent.LoginFlow = (*Adapter)(nil)

// loggedIn reports whether the fake is logged in: always when it has no
// config directory, else once its login accepted a code.
func loggedIn(configDir string) bool {
	if configDir == "" {
		return true
	}
	_, err := os.Stat(filepath.Join(configDir, LoggedInFile))
	return err == nil
}

// LoginLaunch runs the fake's login command on pipes, with the config
// directory sessions use.
func (a *Adapter) LoginLaunch(agent.Spec) (agent.Launch, error) {
	if a.c.ConfigDir == "" {
		return agent.Launch{}, errors.New(Name + ": no config directory to log in to")
	}
	argv := append(append([]string{}, a.c.Command...), "login", "--config", a.c.ConfigDir, "--accept", a.c.Login.Code)
	if a.c.Login.NoLink {
		argv = append(argv, "--no-link")
	}
	if a.c.Login.Quit {
		argv = append(argv, "--quit")
	}
	if a.c.Login.Linger {
		argv = append(argv, "--linger")
	}
	return agent.Launch{Argv: argv, Env: append([]string{}, a.c.Env...)}, nil
}

// LoginDecode reads one line of the login command.
func (a *Adapter) LoginDecode(line []byte) agent.LoginStep {
	l := strings.TrimSpace(string(line))
	switch {
	case strings.HasPrefix(l, loginLinkPrefix):
		return agent.LoginStep{Link: strings.TrimPrefix(l, loginLinkPrefix)}
	case l == loginPrompt:
		return agent.LoginStep{WantsCode: true}
	case l == loginRefused:
		return agent.LoginStep{Refused: l}
	}
	return agent.LoginStep{}
}

// LoginCode is the code and a newline.
func (a *Adapter) LoginCode(code string) ([]byte, error) {
	if code == "" || strings.ContainsAny(code, " \t\r\n") {
		return nil, errors.New(Name + ": a code is one word")
	}
	return []byte(code + "\n"), nil
}

// loginMain is the fake's login command: `login --config DIR --accept CODE
// [--no-link] [--quit]`.
func loginMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("config", "", "")
	accept := fs.String("accept", "", "")
	noLink := fs.Bool("no-link", false, "")
	quit := fs.Bool("quit", false, "")
	linger := fs.Bool("linger", false, "")
	if err := fs.Parse(args); err != nil || *dir == "" {
		return 64
	}
	in := bufio.NewScanner(stdin)
	end := func() int {
		for *linger { // until killed
			time.Sleep(time.Hour)
		}
		return 1
	}
	if *noLink {
		for in.Scan() {
		}
		return end()
	}
	var b [12]byte
	rand.Read(b[:])
	fmt.Fprintf(stdout, "%shttps://login.example.invalid/sign-in?state=%x\n", loginLinkPrefix, b)
	for {
		fmt.Fprintln(stdout, loginPrompt)
		if !in.Scan() {
			return end()
		}
		code := strings.TrimSpace(in.Text())
		if *quit {
			return 0
		}
		if code != "" && code == *accept {
			if err := os.WriteFile(filepath.Join(*dir, LoggedInFile), nil, 0o600); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintln(stdout, loginDone)
			return 0
		}
		fmt.Fprintln(stdout, loginRefused)
	}
}
