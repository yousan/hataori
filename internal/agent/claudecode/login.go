package claudecode

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/yousan/hataori/internal/agent"
)

// The relay logs Claude Code in again with its own login command, on
// pipes, and hands its link and the owner's code through. These functions
// say how to start it and how to read it. They keep nothing and write
// nothing: no file, no log.

var _ agent.LoginFlow = (*Adapter)(nil)

// LoginLaunch returns `claude auth login` on pipes, with the environment a
// session for the same spec gets, so it logs in the same config directory.
// BROWSER is set so that nothing opens a browser on the host.
func (a *Adapter) LoginLaunch(spec agent.Spec) (agent.Launch, error) {
	return agent.Launch{Argv: []string{a.command, "auth", "login"}, Env: append(a.baseEnv(), "BROWSER=true")}, nil
}

var (
	hyperlink = regexp.MustCompile("\x1b\\]8;[^;\x07\x1b]*;[^\x07\x1b]*(?:\x07|\x1b\\\\)") // OSC 8, open or close
	csi       = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
	link      = regexp.MustCompile(`https://\S+`)
)

// What the login command prints (2.1.322, see testdata/2.1.322/login and
// login_terminal).
const (
	askCode      = "Paste code here"
	refusedCode  = "Invalid code"
	visitPreface = "visit:"
)

// LoginDecode reads one line of the login command's output: the sign-in
// link, the request for a code, or the refusal of a code. On a terminal the
// link is wrapped in a hyperlink sequence and coloured; both are removed.
// The request for a code ends without a newline, so the caller also passes
// a last line that is not complete yet.
func (a *Adapter) LoginDecode(line []byte) agent.LoginStep {
	s := csi.ReplaceAllString(hyperlink.ReplaceAllString(string(line), ""), "")
	var st agent.LoginStep
	if i := strings.Index(s, visitPreface); i >= 0 {
		st.Link = link.FindString(s[i:])
	}
	if strings.Contains(s, askCode) {
		st.WantsCode = true
	}
	if i := strings.Index(s, refusedCode); i >= 0 {
		st.Refused = strings.TrimSpace(s[i:])
	}
	return st
}

// ErrNotACode is returned by LoginCode for anything but one word.
var ErrNotACode = errors.New(Name + ": a login code is one word without spaces")

// LoginCode returns the bytes that hand code to the login command: the
// code and a line end. Spaces around it are dropped; anything else that is
// not one printable word is refused.
func (a *Adapter) LoginCode(code string) ([]byte, error) {
	code = strings.TrimSpace(code)
	if code == "" || strings.IndexFunc(code, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return nil, ErrNotACode
	}
	return []byte(code + "\n"), nil
}

// ParseStatus reads the output of `claude auth status`: whether the CLI is
// logged in.
func ParseStatus(out []byte) (loggedIn bool, err error) {
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return false, err
	}
	if st.LoggedIn == nil {
		return false, errors.New(Name + ": auth status has no loggedIn")
	}
	return *st.LoggedIn, nil
}
