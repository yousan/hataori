package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func TestCodexConfig(t *testing.T) {
	for _, s := range []string{"", "{}", `{"command":"  ","sandbox":" ","approval_policy":" "}`} {
		x, err := New([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		a := x.(*Adapter)
		if a.command != "codex" || a.sandbox != "danger-full-access" || a.approvalPolicy != "never" {
			t.Fatal(a)
		}
	}
	x, err := New([]byte(`{"command":" custom ","sandbox":" read-only ","approval_policy":" on-request "}`))
	if err != nil {
		t.Fatal(err)
	}
	a := x.(*Adapter)
	if a.command != "custom" || a.sandbox != "read-only" || a.approvalPolicy != "on-request" {
		t.Fatal(a)
	}
	for _, tc := range []struct{ value, key string }{{`{"foo":1}`, "foo"}, {`{"sandbox":"bad"}`, "sandbox"}, {`{"approval_policy":"bad"}`, "approval_policy"}} {
		_, err := New([]byte(tc.value))
		if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "\n") {
			t.Fatalf("%s: %v", tc.value, err)
		}
	}
}

func TestCodexLaunch(t *testing.T) {
	x, err := New([]byte(`{"command":"custom","pass_env":["SECRET"]}`))
	if err != nil {
		t.Fatalf("New: got %v, want no error", err)
	}
	a := x.(*Adapter)
	a.environ = func() []string {
		return []string{"PATH=p", "CODEX_HOME=h", "SECRET=s", "OPENAI_API_KEY=x", "CODEX_API_KEY=x", "ANTHROPIC_API_KEY=x", "TERM=x", "GIT_DIR=x", "HATAORI_HOST_SOCKET=x"}
	}
	for _, tc := range []struct {
		spec     agent.Spec
		override string
	}{
		{agent.Spec{Dir: "/w/ws.1"}, `projects={"/w/ws.1"={trust_level="trusted"}}`},
		{agent.Spec{Dir: "/w/ws.1", ResumeID: "id", Model: "m", Effort: "e", Instructions: "i"}, `projects={"/w/ws.1"={trust_level="trusted"}}`},
		{agent.Spec{Dir: `/w/a"\b`}, `projects={"/w/a\"\\b"={trust_level="trusted"}}`},
		{agent.Spec{Dir: "/w/del\x7f"}, `projects={"/w/del\u007f"={trust_level="trusted"}}`},
	} {
		l, err := a.Launch(tc.spec)
		if err != nil {
			t.Fatalf("Launch(%+v): got %v, want no error", tc.spec, err)
		}
		want := []string{"custom", "app-server", "-c", tc.override}
		if !reflect.DeepEqual(l.Argv, want) {
			t.Fatalf("argv: got %q, want %q", l.Argv, want)
		}
		if l.Terminal || len(l.Files) != 0 {
			t.Fatalf("terminal and files: got %v and %v, want false and empty", l.Terminal, l.Files)
		}
		env := []string{"PATH=p", "CODEX_HOME=h", "SECRET=s"}
		if !reflect.DeepEqual(l.Env, env) {
			t.Fatalf("environment: got %q, want %q", l.Env, env)
		}
	}
	a.passEnv = nil
	l, err := a.Launch(agent.Spec{Dir: "/w/ws.1"})
	if err != nil {
		t.Fatalf("Launch: got %v, want no error", err)
	}
	wantEnv := []string{"PATH=p", "CODEX_HOME=h"}
	if !reflect.DeepEqual(l.Env, wantEnv) {
		t.Fatalf("environment: got %q, want %q", l.Env, wantEnv)
	}
	for _, dir := range []string{"relative", "/bad\xff"} {
		if _, err := a.Launch(agent.Spec{Dir: dir}); err == nil {
			t.Fatalf("Launch(%q): got nil error, want refusal", dir)
		}
	}
}

func TestCodexProbe(t *testing.T) {
	var captured struct {
		Stdout, Stderr string
		ExitCode       int `json:"exit_code"`
	}
	b, err := os.ReadFile("testdata/0.163.0/login_status/notes.json")
	if err != nil {
		t.Fatalf("read capture: got %v, want no error", err)
	}
	if err = json.Unmarshal(b, &captured); err != nil {
		t.Fatalf("decode capture: got %v, want no error", err)
	}
	version, err := os.ReadFile("testdata/0.163.0/VERSION")
	if err != nil {
		t.Fatalf("read version: got %v, want no error", err)
	}
	first, _, _ := strings.Cut(string(version), "\n")
	dir := t.TempDir()
	stdout, stderr := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	for path, data := range map[string]string{stdout: captured.Stdout, stderr: captured.Stderr} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatalf("write capture: got %v, want no error", err)
		}
	}
	script := filepath.Join(dir, "fake")
	write := func(login string) {
		t.Helper()
		content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%%s\\n' '%s'; else %s; fi\n", first, login)
		if err := os.WriteFile(script, []byte(content), 0700); err != nil {
			t.Fatalf("write script: got %v, want no error", err)
		}
	}
	write(fmt.Sprintf("cat \"%s\"; cat \"%s\" >&2; exit %d", stdout, stderr, captured.ExitCode))
	a := &Adapter{command: script, environ: os.Environ}
	h, err := a.Probe(context.Background(), agent.Spec{})
	if err != nil || !h.Installed || !h.LoggedIn || h.Version != "0.163.0" {
		t.Fatalf("logged-in probe: got %+v, error %v; want installed, logged in, version 0.163.0", h, err)
	}
	// No logged-out capture exists; this output comes from Codex's source.
	write("echo 'Not logged in' >&2; exit 1")
	h, err = a.Probe(context.Background(), agent.Spec{})
	if err != nil || !h.Installed || h.LoggedIn || h.Detail != "Not logged in" {
		t.Fatalf("logged-out probe: got %+v, error %v; want installed, logged out, detail Not logged in", h, err)
	}
	write("exit 1")
	h, err = a.Probe(context.Background(), agent.Spec{})
	if err != nil || !h.Installed || h.LoggedIn || h.Detail != "not logged in" {
		t.Fatalf("silent probe: got %+v, error %v; want installed, logged out, detail not logged in", h, err)
	}
	t.Setenv("HATAORI_TEST_PROBE_LEAK", "1")
	// Record both probe invocations; /usr/bin/env works without PATH.
	record := filepath.Join(dir, "environment")
	content := fmt.Sprintf("#!/bin/sh\n/usr/bin/env >> %q\nif [ \"$1\" = --version ]; then printf 'codex-cli 0.163.0\\n'; fi\n", record)
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatalf("write environment script: %v", err)
	}
	a.command = script
	for _, tc := range []struct {
		name     string
		env      []string
		wantHome bool
	}{
		{"no allowed variables", []string{"OPENAI_API_KEY=x"}, false},
		{"allowed variables", []string{"PATH=" + os.Getenv("PATH"), "CODEX_HOME=h", "OPENAI_API_KEY=x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(record); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			a.environ = func() []string { return tc.env }
			h, err := a.Probe(context.Background(), agent.Spec{})
			if err != nil || !h.Installed || !h.LoggedIn {
				t.Fatalf("probe: got %+v, %v; want installed and logged in", h, err)
			}
			b, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(b), "\n")
			homes := 0
			for _, line := range lines {
				key, _, _ := strings.Cut(line, "=")
				if key == "OPENAI_API_KEY" || key == "HATAORI_TEST_PROBE_LEAK" {
					t.Errorf("probe leaked %s", key)
				}
				if line == "CODEX_HOME=h" {
					homes++
				}
			}
			if tc.wantHome && homes != 2 {
				t.Errorf("CODEX_HOME appeared %d times, want twice: %q", homes, b)
			}
			if !tc.wantHome && homes != 0 {
				t.Errorf("unexpected CODEX_HOME: %q", b)
			}
		})
	}
	a.command = script + "-missing"
	h, err = a.Probe(context.Background(), agent.Spec{})
	if err != nil || h.Installed || h.Detail == "" {
		t.Fatalf("missing command: got %+v, error %v; want not installed with detail", h, err)
	}
}

func TestCodexCodecOptions(t *testing.T) {
	for _, tc := range []struct{ config, sandbox, policy string }{{`{}`, "danger-full-access", "never"}, {`{"sandbox":"read-only","approval_policy":"untrusted"}`, "read-only", "untrusted"}} {
		a, err := New([]byte(tc.config))
		if err != nil {
			t.Fatal(err)
		}
		actions := a.NewCodec(agent.Spec{Dir: "/w"}).Open()
		found := false
		for _, b := range actions.Write {
			var m struct {
				Method string
				Params map[string]any
			}
			if json.Unmarshal(b, &m) == nil && m.Method == "thread/start" {
				found = true
				if m.Params["sandbox"] != tc.sandbox || m.Params["approvalPolicy"] != tc.policy {
					t.Fatal(m)
				}
			}
		}
		if !found {
			t.Fatal("missing thread/start")
		}
	}
}

func TestWorkerProbeAndLaunchUseOnlyLocalLoginEnvironment(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "environment")
	script := filepath.Join(dir, "probe-fixture")
	version, err := os.ReadFile("testdata/0.163.0/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(version), "\n")
	var status struct {
		Stdout, Stderr string
		ExitCode       int `json:"exit_code"`
	}
	b, err := os.ReadFile("testdata/0.163.0/login_status/notes.json")
	if err != nil || json.Unmarshal(b, &status) != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte(status.Stdout), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(status.Stderr), 0600); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("#!/bin/sh\n/usr/bin/env >> %q\nif [ \"$1\" = --version ]; then printf '%%s\\n' '%s'; else /bin/cat %q; /bin/cat %q >&2; exit %d; fi\n", record, first, filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr"), status.ExitCode)
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{command: script, passEnv: []string{"GITHUB_TOKEN", "BOT_MARKER", "SUPERVISOR_MARKER", "SSH_AUTH_SOCK"}, environ: func() []string {
		return []string{"HOME=" + dir, "CODEX_HOME=/default-login", "GITHUB_TOKEN=secret", "BOT_MARKER=secret", "SUPERVISOR_MARKER=secret", "SSH_AUTH_SOCK=secret"}
	}}
	local := a.ForWorker()
	spec := agent.Spec{Dir: dir, ConfigDir: filepath.Join(dir, "alternate-login")}
	h, err := local.Probe(context.Background(), spec)
	if err != nil || !h.Installed || !h.LoggedIn || h.Unknown {
		t.Fatalf("probe: %+v %v", h, err)
	}
	launch, err := local.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	probeEnv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{string(probeEnv), strings.Join(launch.Env, "\n")} {
		if !strings.Contains(env, "CODEX_HOME="+spec.ConfigDir) || strings.Contains(env, "/default-login") || strings.Contains(env, "secret") {
			t.Fatalf("incorrect worker environment: %s", env)
		}
	}
	if len(a.passEnv) != 4 {
		t.Fatal("ForWorker mutated the main adapter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h, err = local.Probe(ctx, spec)
	if err != nil || h.Installed || !h.Unknown {
		t.Fatalf("cancelled probe claimed missing installation: %+v %v", h, err)
	}
}
