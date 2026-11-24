package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// Name is the name the adapter is registered under.
const Name = "claude-code"

// Config is the adapter's section of the instance's config.
type Config struct {
	// Command is the CLI to run; "claude" on the PATH by default.
	Command string `json:"command"`
	// Hataori is the hataori binary the hooks run; this binary by default.
	Hataori string `json:"hataori"`
	// HookTimeout bounds how long a hook that waits for an answer (a
	// question, an approval) waits, as a Go duration; 24h by default.
	HookTimeout string `json:"hook_timeout"`
	// InputTimeout bounds how long input may go unreported by the CLI's
	// UserPromptSubmit hook before it counts as not delivered; 30s by default.
	InputTimeout string `json:"input_timeout"`
	// PassEnv names variables of the daemon's environment to pass on
	// besides the fixed list, for example an API key the CLI logs in with.
	PassEnv []string `json:"pass_env"`
}

// Adapter runs Claude Code.
type Adapter struct {
	command      string
	hataori      string
	hookTimeout  time.Duration
	inputTimeout time.Duration
	passEnv      []string

	environ func() []string // the daemon's environment; os.Environ unless a test sets it
}

var _ agent.Adapter = (*Adapter)(nil)

// New makes the adapter from its config section (JSON; may be empty).
func New(settings []byte) (agent.Adapter, error) {
	var c Config
	if len(bytes.TrimSpace(settings)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: config: %w", Name, err)
		}
	}
	a := &Adapter{command: strings.TrimSpace(c.Command), hataori: strings.TrimSpace(c.Hataori),
		hookTimeout: 24 * time.Hour, passEnv: c.PassEnv, environ: os.Environ}
	if a.command == "" {
		a.command = "claude"
	}
	if a.hataori == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", Name, err)
		}
		a.hataori = exe
	}
	if t := strings.TrimSpace(c.InputTimeout); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("%s: config: input_timeout %q is not a duration of a second or more", Name, t)
		}
		a.inputTimeout = d
	}
	if t := strings.TrimSpace(c.HookTimeout); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("%s: config: hook_timeout %q is not a duration of a second or more", Name, t)
		}
		a.hookTimeout = d
	}
	return a, nil
}

// Files Launch asks the host to write into the session's state directory.
const (
	SettingsFile     = "claude-settings.json"
	InstructionsFile = "claude-instructions.md"
	MCPConfigFile    = "claude-mcp.json" // a confined session's only MCP servers
)

// ConfinedTools are the built-in tools a confined session keeps: the file
// tools, which --restricted confines to the working directory. The set and
// the flags of Launch are the ones checked against Claude Code 2.1.327
// (design, Appendix B, "Confining a supervisor session").
const ConfinedTools = "Read,Write,Edit,Glob,Grep"

// ConfinedEnv is added to a confined session's environment, so that what
// the session can write in its own directory reaches nothing: no CLAUDE.md
// files, no automatic memory and no claude.ai connectors, as the real
// checks run the CLI, and no git repository, since a .git it wrote there
// could name commands that git runs, as the CLI's own look at the
// directory at its start would.
var ConfinedEnv = []string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	"ENABLE_CLAUDEAI_MCP_SERVERS=false", "GIT_DIR=/dev/null"}

// baseEnv is the fixed list of variables passed from the daemon's
// environment. Nothing else is, unless the config's pass_env names it.
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
	"TZ", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR"}

// Launch returns the interactive CLI on a terminal, with the hooks of this
// session registered by --settings. A new session gets a fresh
// --session-id; a resumed one --resume. Permission requests are answered
// by the hooks, so the CLI never runs in its bypass mode.
func (a *Adapter) Launch(spec agent.Spec) (agent.Launch, error) {
	// The CLI runs in the workspace, so a relative path would point there.
	if !filepath.IsAbs(spec.StateDir) {
		return agent.Launch{}, fmt.Errorf("%s: state directory %q is not an absolute path", Name, spec.StateDir)
	}
	settings, err := a.settings(spec)
	if err != nil {
		return agent.Launch{}, err
	}
	files := map[string][]byte{SettingsFile: settings}
	argv := []string{a.command, "--settings", stateFile(spec, SettingsFile), "--permission-mode", "default"}
	if spec.ResumeID != "" {
		argv = append(argv, "--resume", spec.ResumeID)
	} else {
		argv = append(argv, "--session-id", newUUID())
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.Instructions != "" {
		files[InstructionsFile] = []byte(spec.Instructions)
		argv = append(argv, "--append-system-prompt-file", stateFile(spec, InstructionsFile))
	}
	l := agent.Launch{Argv: argv, Env: a.env(spec), Terminal: true, Files: files}
	if spec.NoProjectSettings && spec.Confine == nil {
		// The project's and the local settings stay out, with their hooks:
		// every launch of the spec, a --resume too (#590).
		l.Argv = append(l.Argv, "--setting-sources", "user")
		l.NoProjectSettings = true
	}
	if spec.Confine != nil {
		// The flags belong to each launch: a --resume without them has a
		// shell again (Appendix B), so every launch of the spec has them.
		mcp, err := mcpConfig(spec)
		if err != nil {
			return agent.Launch{}, err
		}
		files[MCPConfigFile] = mcp
		// --setting-sources user: the project's and the local settings are
		// files in its own directory, which it can write; they stay out by
		// the flag, not only by --restricted.
		l.Argv = append(l.Argv, "--restricted", "--strict-mcp-config", "--mcp-config", stateFile(spec, MCPConfigFile),
			"--tools", ConfinedTools, "--setting-sources", "user")
		l.Env = append(l.Env, ConfinedEnv...)
		l.Confined = true
	}
	return l, nil
}

// mcpConfig is the file given with --mcp-config: the spec's servers, each
// on stdio, and those that report with what they hand to the session's
// host (mcpwait.go).
func mcpConfig(spec agent.Spec) ([]byte, error) {
	type server struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env,omitempty"`
	}
	all := map[string]server{}
	for _, s := range spec.Confine.MCP {
		if s.Name == "" || len(s.Argv) == 0 {
			return nil, fmt.Errorf("%s: an MCP server needs a name and a command", Name)
		}
		if _, dup := all[s.Name]; dup {
			return nil, fmt.Errorf("%s: MCP server %q is named twice", Name, s.Name)
		}
		v := server{Type: "stdio", Command: s.Argv[0], Args: append([]string{}, s.Argv[1:]...)}
		env := s.Env
		if s.Reports {
			env = append(slices.Clone(env), host.HookEnv(spec.StateDir), agent.MCPReportEnv+"="+mcpReport(s.Name))
		}
		for _, kv := range env {
			k, val, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("%s: MCP server %q: %q is not NAME=value", Name, s.Name, kv)
			}
			if v.Env == nil {
				v.Env = map[string]string{}
			}
			v.Env[k] = val
		}
		all[s.Name] = v
	}
	return json.MarshalIndent(map[string]any{"mcpServers": all}, "", "  ")
}

func stateFile(spec agent.Spec, name string) string {
	return strings.TrimSuffix(spec.StateDir, "/") + "/" + name
}

// env builds the CLI's environment from the fixed list, the config's
// pass_env, a terminal type and where the hooks find the host.
func (a *Adapter) env(spec agent.Spec) []string {
	env := a.baseEnv()
	if spec.ConfigDir != "" {
		// The configuration directory the session ran with, such as the one
		// of a window it was taken in from: instead of the daemon's.
		env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") })
		env = append(env, "CLAUDE_CONFIG_DIR="+spec.ConfigDir)
	}
	return append(env, host.HookEnv(spec.StateDir))
}

// baseEnv is the environment without the hooks' variable: the fixed list,
// pass_env, and a terminal type.
func (a *Adapter) baseEnv() []string {
	allowed := map[string]bool{}
	for _, k := range append(append([]string{}, baseEnv...), a.passEnv...) {
		allowed[k] = true
	}
	var env []string
	for _, kv := range a.environ() {
		k, _, _ := strings.Cut(kv, "=")
		if allowed[k] {
			env = append(env, kv)
		}
	}
	return append(env, "TERM=xterm-256color")
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Probe reports the installed version and whether the CLI is logged in. It
// runs only `--version` and `auth status`, which read and change nothing.
func (a *Adapter) Probe(ctx context.Context) (agent.Health, error) {
	var h agent.Health
	out, err := a.run(ctx, "--version")
	if err != nil {
		h.Detail = "not installed: " + err.Error()
		return h, nil
	}
	h.Installed = true
	h.Version, _, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	out, err = a.run(ctx, "auth", "status")
	loggedIn, jerr := ParseStatus(out)
	if jerr != nil {
		if err == nil {
			err = jerr
		}
		h.Detail = "auth status: " + err.Error()
		return h, nil
	}
	h.LoggedIn = loggedIn
	if !loggedIn {
		h.Detail = "not logged in"
	}
	return h, nil
}

func (a *Adapter) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.command, args...)
	cmd.Env = a.baseEnv()
	cmd.Dir = os.TempDir()
	return cmd.Output()
}
