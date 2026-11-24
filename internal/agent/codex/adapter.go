package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/agent"
)

// Name is the adapter's registration name.
const Name = "codex"

// Config is the adapter's section of the instance configuration.
type Config struct {
	// Command is the CLI to run; "codex" on PATH by default.
	Command string `json:"command"`
	// PassEnv names additional daemon environment variables to pass; empty by default.
	PassEnv []string `json:"pass_env"`
	// Sandbox is the app-server sandbox; "danger-full-access" by default.
	Sandbox string `json:"sandbox"`
	// ApprovalPolicy is the app-server approval policy; "never" by default.
	ApprovalPolicy string `json:"approval_policy"`
}

// Adapter runs Codex app-server on pipes.
type Adapter struct {
	command, sandbox, approvalPolicy string
	passEnv                          []string
	environ                          func() []string
}

// New constructs an adapter from its JSON configuration.
func New(settings []byte) (agent.Adapter, error) {
	var c Config
	if len(bytes.TrimSpace(settings)) != 0 {
		dec := json.NewDecoder(bytes.NewReader(settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: config: %w", Name, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("%s: config: unexpected trailing value", Name)
		}
	}
	a := &Adapter{command: strings.TrimSpace(c.Command), sandbox: strings.TrimSpace(c.Sandbox), approvalPolicy: strings.TrimSpace(c.ApprovalPolicy), passEnv: c.PassEnv, environ: os.Environ}
	if a.command == "" {
		a.command = "codex"
	}
	if a.sandbox == "" {
		a.sandbox = "danger-full-access"
	}
	if a.approvalPolicy == "" {
		a.approvalPolicy = "never"
	}
	switch a.sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return nil, fmt.Errorf("%s: config: sandbox %q is invalid", Name, a.sandbox)
	}
	switch a.approvalPolicy {
	case "untrusted", "on-request", "never":
	default:
		return nil, fmt.Errorf("%s: config: approval_policy %q is invalid", Name, a.approvalPolicy)
	}
	return a, nil
}

// baseEnv is the fixed list of variables passed from the daemon's
// environment. Nothing else is, unless pass_env names it.
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "CODEX_HOME"}

// env selects the fixed environment and variables named in pass_env.
func (a *Adapter) env() []string {
	allowed := make(map[string]bool)
	for _, k := range baseEnv {
		allowed[k] = true
	}
	for _, k := range a.passEnv {
		allowed[k] = true
	}
	env := make([]string, 0)
	for _, kv := range a.environ() {
		k, _, _ := strings.Cut(kv, "=")
		if allowed[k] {
			env = append(env, kv)
		}
	}
	return env
}

// tomlString uses JSON string encoding, with DEL escaped because TOML basic
// strings forbid it unescaped while JSON permits it.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return strings.ReplaceAll(string(b), "\x7f", `\u007f`)
}

// Launch starts app-server on pipes. The trust override prevents Codex from
// recording the workspace in the user's configuration. The path is in the
// value because Codex splits keys on dots, which paths may contain. A resumed
// session gets the same command; the codec chooses thread/resume.
func (a *Adapter) Launch(spec agent.Spec) (agent.Launch, error) {
	if !filepath.IsAbs(spec.Dir) || !utf8.ValidString(spec.Dir) {
		return agent.Launch{}, fmt.Errorf("%s: workspace %q must be an absolute UTF-8 path", Name, spec.Dir)
	}
	return agent.Launch{Argv: []string{a.command, "app-server", "-c", "projects={" + tomlString(spec.Dir) + "={trust_level=\"trusted\"}}"}, Env: a.env()}, nil
}

// Probe runs only --version and login status, which read and change nothing.
var _ agent.LoginDirs = (*Adapter)(nil)

// LoginDir is always "": every session runs with the daemon's CODEX_HOME.
func (a *Adapter) LoginDir(agent.Spec) string { return "" }

func (a *Adapter) Probe(ctx context.Context, _ agent.Spec) (agent.Health, error) {
	var h agent.Health
	out, _, err := a.run(ctx, "--version")
	if err != nil {
		h.Detail = "not installed: " + err.Error()
		return h, nil
	}
	h.Installed = true
	h.Version = strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli ")
	_, stderr, err := a.run(ctx, "login", "status")
	if err == nil {
		h.LoggedIn = true
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			h.Detail = strings.TrimSpace(string(stderr))
			if h.Detail == "" {
				h.Detail = "not logged in"
			}
		} else {
			h.Detail = err.Error()
		}
	}
	return h, nil
}

// run executes a probe with the session environment in the temporary directory.
func (a *Adapter) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.command, args...)
	cmd.Env = a.env()
	cmd.Dir = os.TempDir()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, stderr.Bytes(), err
}

// NewCodec returns a codec configured with the adapter's policies.
func (a *Adapter) NewCodec(spec agent.Spec) agent.Codec {
	return NewCodec(spec, Options{Sandbox: a.sandbox, ApprovalPolicy: a.approvalPolicy})
}
