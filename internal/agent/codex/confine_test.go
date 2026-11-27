package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// Use the actual CLI's literal-input userMessage, not a hand-written event.
func TestConfinedLiteralInputFromCurrentCLI(t *testing.T) {
	lines := replay(t, "supervisor_gate")
	var response, message []byte
	var text string
	for _, l := range lines {
		m := parse(t, l.Msg)
		if response == nil && l.Dir == "recv" && m.Result.Thread != nil {
			var v map[string]any
			json.Unmarshal(l.Msg, &v)
			v["id"] = 2
			response, _ = json.Marshal(v)
		}
		if m.Method == "item/started" {
			var p params
			json.Unmarshal(l.Msg, &struct {
				Params *params `json:"params"`
			}{&p})
			if p.Item.Type == "userMessage" && len(p.Item.Content) > 0 && strings.HasPrefix(p.Item.Content[0].Text, "/model unsafe-model") {
				message = l.Msg
				text = p.Item.Content[0].Text
				break
			}
		}
	}
	if response == nil || message == nil {
		t.Fatal("current CLI capture has no literal input evidence")
	}
	original := strings.NewReplacer("(at)", "@", "(dollar)", "$").Replace(text)
	for _, resume := range []bool{false, true} {
		state := t.TempDir()
		os.MkdirAll(filepath.Join(state, confinedHome), 0700)
		os.WriteFile(filepath.Join(state, confinedInstructions), []byte(confinedInstructionsHeader+"full-composed-instructions"), 0600)
		spec := agent.Spec{StateDir: state, Dir: "/fixture/work", Model: confinedModel, Effort: "low", Confine: &agent.Confinement{}}
		if resume {
			spec.ResumeID = parse(t, response).Result.Thread.ID
		}
		c := NewCodec(spec, Options{Sandbox: "danger-full-access", ApprovalPolicy: "on-request", confinedInstructions: readConfinedInstructions(state)}).(*codec)
		opened := c.Open()
		p := parse(t, opened.Write[2]).Params
		if p["developerInstructions"] != "full-composed-instructions" {
			t.Fatal("codec lost composed launch instructions")
		}
		if p["sandbox"] != "read-only" || p["approvalPolicy"] != "never" {
			t.Fatal("ordinary policies weakened confinement", p)
		}
		if !resume {
			if env, ok := p["environments"].([]any); !ok || len(env) != 0 {
				t.Fatal("start has a native environment", p)
			}
		}
		injected := c.Decode(agent.Output, response)
		if len(injected.Write) != 1 || parse(t, injected.Write[0]).Method != "thread/inject_items" {
			t.Fatal("current instructions were not injected before readiness", injected)
		}
		request := parse(t, injected.Write[0])
		ack, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{}})
		c.Decode(agent.Output, ack)
		a, err := c.Input(agent.Input{ID: "literal", Text: original})
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Write) == 0 {
			t.Fatalf("no turn emitted: %+v", a)
		}
		turn := parse(t, a.Write[0]).Params
		if env, ok := turn["environments"].([]any); !ok || len(env) != 0 {
			t.Fatal("turn has a native environment", turn)
		}
		actual := turn["input"].([]any)[0].(map[string]any)
		if actual["text"] != text {
			t.Fatalf("input differs from real safe capture: %q", actual["text"])
		}
		if elements, ok := actual["text_elements"].([]any); !ok || len(elements) != 0 {
			t.Fatal("input can attach resources")
		}
		events := c.Decode(agent.Output, message).Events
		matched := false
		for _, e := range events {
			if e.Kind == agent.TurnStarted && e.Input == "literal" {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("neutralized input lost its delivery identity: %+v", events)
		}
		// Steering the same text must not reactivate file/skill references.
		c.ready, c.running, c.started = true, true, true
		c.turnID = "running"
		a, err = c.Input(agent.Input{ID: "steer", Text: original})
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Write) == 0 {
			t.Fatal("steer was not emitted")
		}
		steer := parse(t, a.Write[0])
		if steer.Method != "turn/steer" || steer.Params["input"].([]any)[0].(map[string]any)["text"] != text {
			t.Fatal("steered text can attach a resource", steer)
		}
	}
}

func TestConfinedLaunchIsolationAndRefusals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux launch only")
	}
	root := t.TempDir()
	login := filepath.Join(root, "login")
	state := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	for _, d := range []string{login, state, work} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	auth := filepath.Join(login, "auth.json")
	os.WriteFile(auth, []byte("synthetic-auth-do-not-overwrite"), 0600)
	cli := filepath.Join(root, "codex")
	os.WriteFile(cli, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'codex-cli 0.163.0\\n'; else printf 'Logged in using ChatGPT\\n' >&2; fi\n"), 0700)
	// No bubblewrap is executed by this contract test.
	os.WriteFile(filepath.Join(root, "bwrap"), []byte("#!/bin/sh\nexit 99\n"), 0700)
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	x, _ := New(nil)
	a := x.(*Adapter)
	a.command = cli
	a.environ = func() []string {
		return []string{"HOME=" + root, "CODEX_HOME=" + login, "OPENAI_API_KEY=must-not-pass", "CODEX_API_KEY=must-not-pass"}
	}
	spec := agent.Spec{Dir: work, StateDir: state, Model: confinedModel, Instructions: "SCOPE-INSTRUCTIONS-MARKER", Confine: &agent.Confinement{MCP: []agent.MCPServer{{Name: "sv", Argv: []string{cli}, Env: []string{"FIXTURE_CREDENTIAL=fixture-only"}}}}}
	launch, err := a.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !launch.Confined || !launch.NoWriteTools || !launch.NoCommands || !launch.NoMentions {
		t.Fatal("no confinement attestations")
	}
	config := string(launch.Files[confinedHome+"/config.toml"])
	if !strings.Contains(string(launch.Files[confinedInstructions]), "SCOPE-INSTRUCTIONS-MARKER") {
		t.Fatal("composed launch instructions lost")
	}
	if !strings.Contains(config, `"FIXTURE_CREDENTIAL"="fixture-only"`) || strings.Contains(strings.Join(launch.Argv, " "), "fixture-only") {
		t.Fatal("supplied MCP credential missing or exposed in argv")
	}
	if _, ok := launch.Files[confinedHome+"/auth.json"]; ok {
		t.Fatal("launch would overwrite authentication")
	}
	if strings.Contains(strings.Join(launch.Env, " "), "must-not-pass") {
		t.Fatal("ambient API credentials reached launch")
	}
	argv := strings.Join(launch.Argv, " ")
	if !strings.Contains(argv, "--ro-bind "+auth+" "+filepath.Join(state, confinedHome, "auth.json")) {
		t.Fatal("authentication is not mounted read-only")
	}
	for _, change := range []func(*agent.Spec){func(s *agent.Spec) { s.Model = "unverified" }, func(s *agent.Spec) { s.StateDir = s.Dir }, func(s *agent.Spec) { s.Confine = &agent.Confinement{MCP: []agent.MCPServer{{Name: "sv"}}} }} {
		bad := spec
		change(&bad)
		if l, err := a.Launch(bad); err == nil || l.Confined {
			t.Fatal("unsafe launch accepted")
		}
	}
	home := filepath.Join(state, confinedHome)
	if err := os.Symlink(login, home); err != nil {
		t.Fatal(err)
	}
	if l, err := a.Launch(spec); err == nil || l.Confined {
		t.Fatal("symlink state could overwrite existing login")
	}
	b, _ := os.ReadFile(auth)
	if string(b) != "synthetic-auth-do-not-overwrite" {
		t.Fatal("existing authentication changed")
	}
}

func TestConfinedAsyncAnswerUsesLiteralText(t *testing.T) {
	r := newRequestReplay(t, "question_async", agent.AllowAll)
	r.c.(*codec).spec.Confine = &agent.Confinement{}
	for {
		_, actions, ok := r.next()
		if !ok {
			t.Fatal("real capture contains no async question")
		}
		if question := requestEvent(t, actions); question != nil {
			answer, err := r.c.Answer(agent.Answer{Request: question.ID, Text: "@/fixture/work/mention.txt $openai-docs"})
			if err != nil {
				t.Fatal(err)
			}
			if len(answer.Write) == 0 {
				t.Fatal("no async answer steer")
			}
			message := parse(t, answer.Write[0])
			input := message.Params["input"].([]any)[0].(map[string]any)
			if message.Method != "turn/steer" || input["text"] != "(at)/fixture/work/mention.txt (dollar)openai-docs" {
				t.Fatal("answer reactivated resource mentions", message)
			}
			if elements, ok := input["text_elements"].([]any); !ok || len(elements) != 0 {
				t.Fatal("answer can attach resources")
			}
			break
		}
	}
}

func TestConfinedInstructionsFailClosed(t *testing.T) {
	root := t.TempDir()
	a, _ := New(nil)
	spec := agent.Spec{StateDir: root, Confine: &agent.Confinement{}}
	assertBlocked := func() {
		t.Helper()
		opened := a.NewCodec(spec).Open()
		if len(opened.Write) != 0 || len(opened.Events) != 1 || opened.Events[0].Kind != agent.SessionEnded || opened.Events[0].Reason != agent.BlockedAtStart {
			t.Fatal("invalid instructions started a session", opened)
		}
	}
	assertBlocked()
	os.MkdirAll(filepath.Join(root, confinedHome), 0700)
	path := filepath.Join(root, confinedInstructions)
	os.WriteFile(path, []byte("unrecognized"), 0600)
	assertBlocked()
	os.WriteFile(path, []byte(confinedInstructionsHeader+"current-user-layer"), 0600)
	opened := a.NewCodec(spec).Open()
	if len(opened.Write) < 3 || parse(t, opened.Write[2]).Params["developerInstructions"] != "current-user-layer" {
		t.Fatal("current composed instructions lost", opened)
	}
	// Preserve the content, but refuse a hard link at the reader boundary.
	os.Link(path, filepath.Join(root, "linked-instructions"))
	assertBlocked()
}
