package fake

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
	// Login is what its login command does.
	Login Login `json:"login"`
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
	// the program never report that it is ready.
	for _, l := range strings.Split(spec.Instructions, "\n") {
		if strings.TrimSpace(l) == "never-start" {
			argv = append(argv, "--never-start")
		}
	}
	if a.c.Conversations != "" {
		argv = append(argv, "--conversations", a.c.Conversations)
	}
	if a.c.ConfigDir != "" {
		argv = append(argv, "--config", a.c.ConfigDir)
	}
	env := append([]string{}, a.c.Env...)
	if a.c.Terminal {
		argv = append(argv, "--terminal", "--hook", a.c.Hataori,
			"--transcript", filepath.Join(spec.StateDir, TranscriptFile))
		env = append(env, host.HookEnv(spec.StateDir))
	}
	return agent.Launch{Argv: argv, Env: env, Terminal: a.c.Terminal}, nil
}

// NewCodec returns the codec of one session.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec {
	return &codec{terminal: a.c.Terminal, followed: map[string]bool{}}
}
