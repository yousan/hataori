package fake

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// Name is the name the fake registers under in tests.
const Name = "fake"

// Config is the fake's section of the instance's config.
type Config struct {
	// Command runs the program: a binary built from ./fakeagent, or a test
	// binary that calls Main.
	Command []string `json:"command"`
	// Hataori is the binary whose `hook` the terminal form runs.
	Hataori string `json:"hataori"`
	// Terminal selects the terminal form; pipes otherwise.
	Terminal bool `json:"terminal"`
	// Env is passed to the program, for example PATH for its tool.
	Env []string `json:"env"`
	// Conversations, when set, is a directory where the program keeps one
	// file per session it started. Resuming a session whose file is gone
	// fails as "cannot resume", as when an agent's retention removed it.
	Conversations string `json:"conversations"`
	// ConfigDir, when set, is where the fake keeps its login, like an
	// agent's config directory: until its login command has accepted a
	// code, every turn fails with the reason login.
	ConfigDir string `json:"config_dir"`
	// Record, when set, is a directory where the program writes, at each
	// start, its arguments, environment and directory: one file per start.
	Record string `json:"record"`
	// Login is what its login command does.
	Login Login `json:"login"`
	// InputTimeout is how long written input may go unreported before its
	// turn fails as "not delivered"; DefaultInputTimeout when empty.
	InputTimeout string `json:"input_timeout"`
	// Dialog, in the terminal form, is the text of a start-up dialog that
	// no hook reports: the program shows it, and after an Enter writes a
	// line and then reports its start; it shows the dialog once more after
	// the start. The codec watches the screen for it, answers it with
	// Enter, and watches on until its first turn ends.
	Dialog string `json:"dialog"`
	// DialogTrust are files the dialog names, as a folder-trust dialog
	// names the project files trusting the folder loads: the codec answers
	// it only when the session's Spec.TrustFiles lists all of them, and
	// otherwise ends the start as blocked at start, naming them.
	DialogTrust []string `json:"dialog_trust"`
	// ReportModel and ReportEffort, when set, are what the program reports
	// when it is ready, instead of the model and effort it was asked for.
	ReportModel  string `json:"report_model"`
	ReportEffort string `json:"report_effort"`
	// Confines makes the fake take a confined spec: it confines nothing,
	// but starts the spec's MCP servers for the script command "mcp", as an
	// agent would. Without it a confined spec's launch is not confined, and
	// the runner refuses it.
	Confines bool `json:"confines"`
	// Cheap is the model the fake names as its cheapest; none when empty.
	Cheap string `json:"cheap_model"`
}

// Adapter is the fake agent.
type Adapter struct{ c Config }

var _ agent.Adapter = (*Adapter)(nil)

// New makes the fake from its config section (JSON).
func New(settings []byte) (agent.Adapter, error) {
	var c Config
	if len(bytes.TrimSpace(settings)) > 0 {
		if err := json.Unmarshal(settings, &c); err != nil {
			return nil, fmt.Errorf("%s: config: %w", Name, err)
		}
	}
	if len(c.Command) == 0 {
		return nil, fmt.Errorf("%s: config: no command", Name)
	}
	if c.ConfigDir != "" && !filepath.IsAbs(c.ConfigDir) {
		// Sessions run in their workspace, the login and Probe elsewhere.
		return nil, fmt.Errorf("%s: config: config_dir %q is not an absolute path", Name, c.ConfigDir)
	}
	if t := c.InputTimeout; t != "" {
		if d, err := time.ParseDuration(t); err != nil || d <= 0 {
			return nil, fmt.Errorf("%s: config: input_timeout %q is not a positive duration", Name, t)
		}
	}
	return &Adapter{c: c}, nil
}

// Probe reports the fake installed, and logged in unless it has a config
// directory whose login has not succeeded.
func (a *Adapter) Probe(context.Context) (agent.Health, error) {
	h := agent.Health{Installed: true, LoggedIn: loggedIn(a.c.ConfigDir), Version: "fake"}
	if !h.LoggedIn {
		h.Detail = "not logged in"
	}
	return h, nil
}

// TranscriptFile is where the terminal form writes its text, in the
// session's state directory.
const TranscriptFile = "fake-transcript.jsonl"

// MCPConfigFile holds a confined spec's MCP servers, in the session's state
// directory, as agent.MCPServer values in JSON.
const MCPConfigFile = "fake-mcp.json"

// CheapModel is the configured cheap_model.
func (a *Adapter) CheapModel() string { return a.c.Cheap }

var _ agent.CheapModel = (*Adapter)(nil)

// Launch runs the program with the spec's model, effort and resume id.
func (a *Adapter) Launch(spec agent.Spec) (agent.Launch, error) {
	argv := append([]string{}, a.c.Command...)
	argv = append(argv, "--model", spec.Model, "--effort", spec.Effort)
	if spec.ResumeID != "" {
		argv = append(argv, "--resume", spec.ResumeID)
	} else {
		var b [8]byte
		rand.Read(b[:])
		argv = append(argv, "--session", fmt.Sprintf("fake-%x", b))
	}
	// The start-up script is the spec's instructions: "never-start" makes
	// the program never report that it is ready; "input-delay D" makes it
	// wait D before it takes each input; "relay A B" makes it relay
	// between threads A and B.
	for _, l := range strings.Split(spec.Instructions, "\n") {
		l = strings.TrimSpace(l)
		if l == "never-start" {
			argv = append(argv, "--never-start")
		}
		if d, ok := strings.CutPrefix(l, "input-delay "); ok {
			argv = append(argv, "--input-delay", strings.TrimSpace(d))
		}
		if r, ok := strings.CutPrefix(l, "relay "); ok {
			if f := strings.Fields(r); len(f) == 2 {
				argv = append(argv, "--relay", f[0]+","+f[1])
			}
		}
	}
	if a.c.Conversations != "" {
		argv = append(argv, "--conversations", a.c.Conversations)
	}
	if dir := cmp.Or(spec.ConfigDir, a.c.ConfigDir); dir != "" {
		argv = append(argv, "--config", dir)
	}
	if a.c.Record != "" {
		argv = append(argv, "--record", a.c.Record)
	}
	if a.c.ReportModel != "" {
		argv = append(argv, "--report-model", a.c.ReportModel)
	}
	if a.c.ReportEffort != "" {
		argv = append(argv, "--report-effort", a.c.ReportEffort)
	}
	if a.c.Terminal && a.c.Dialog != "" {
		argv = append(argv, "--dialog", a.c.Dialog)
	}
	env := append([]string{}, a.c.Env...)
	if a.c.Terminal {
		argv = append(argv, "--terminal", "--hook", a.c.Hataori,
			"--transcript", filepath.Join(spec.StateDir, TranscriptFile))
		env = append(env, host.HookEnv(spec.StateDir))
	}
	l := agent.Launch{Argv: argv, Env: env, Terminal: a.c.Terminal}
	if spec.Confine != nil && a.c.Confines {
		b, err := json.Marshal(spec.Confine.MCP)
		if err != nil {
			return agent.Launch{}, err
		}
		l.Files = map[string][]byte{MCPConfigFile: b}
		l.Argv = append(l.Argv, "--mcp-config", filepath.Join(spec.StateDir, MCPConfigFile))
		l.Confined = true
	}
	return l, nil
}

// NewCodec returns the codec of one session.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec {
	d, _ := time.ParseDuration(a.c.InputTimeout) // checked in New
	c := &codec{terminal: a.c.Terminal, followed: map[string]bool{}, timeout: d}
	if a.c.Terminal && !spec.TakenIn { // a session taken in is past any dialog
		c.dialog = a.c.Dialog
		for _, f := range a.c.DialogTrust {
			if !slices.Contains(spec.TrustFiles, f) {
				c.trust = a.c.DialogTrust
			}
		}
	}
	if spec.TakenIn {
		c.takenIn, c.session, c.model, c.started = true, spec.ResumeID, spec.Model, true
	}
	return c
}
