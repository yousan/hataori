package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
		ack := instructionAck(t, request.ID)
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

func instructionAck(t *testing.T, id int) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/0.163.0/supervisor_actual/instruction-ack.json")
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(b, &response); err != nil {
		t.Fatal(err)
	}
	response["id"] = id // The same real response, correlated to this replay.
	b, _ = json.Marshal(response)
	return b
}

func TestConfinedInstructionAckReplayOrdering(t *testing.T) {
	var thread, notification []byte
	for _, line := range replay(t, "start") {
		m := parse(t, line.Msg)
		if line.Dir == "recv" && m.Result.Thread != nil {
			thread = line.Msg
		}
		if m.Method == RateLimitsUpdated {
			notification = line.Msg
		}
	}
	limits, err := os.ReadFile(filepath.Join(captures, "codex-ratelimits-read.json"))
	if err != nil || thread == nil || notification == nil {
		t.Fatal("missing captured startup/rate-limit responses", err)
	}
	response := func(id int) []byte {
		b, _ := json.Marshal(map[string]any{"id": id, "result": json.RawMessage(limits)})
		return b
	}
	for _, opened := range []bool{false, true} {
		for _, notify := range []bool{false, true} {
			for _, limitsFirst := range []bool{false, true} {
				c := NewCodec(agent.Spec{Model: confinedModel, Confine: &agent.Confinement{}}, Options{confinedInstructions: "current-composed-instructions"}).(*codec)
				if opened {
					c.Open()
				}
				c.Input(agent.Input{ID: "queued", Text: "literal"})
				id := 4
				if notify {
					c.Decode(agent.Output, notification)
					id++
				}
				if limitsFirst {
					c.Decode(agent.Output, response(3))
				}
				injected := c.Decode(agent.Output, thread)
				if len(injected.Write) != 1 {
					t.Fatal("no single instruction injection", injected)
				}
				request := parse(t, injected.Write[0])
				if request.ID != id || request.Method != "thread/inject_items" || !strings.Contains(string(injected.Write[0]), "current-composed-instructions") {
					t.Fatalf("handshake id mismatch: open=%v notify=%v limitsFirst=%v: %s", opened, notify, limitsFirst, injected.Write[0])
				}
				if !limitsFirst {
					c.Decode(agent.Output, response(3))
				}
				if notify {
					c.Decode(agent.Output, response(4))
				}
				if c.ready || c.running {
					t.Fatal("rate-limit response released instructions gate")
				}
				retry := c.Tick(time.Now())
				if opened {
					if len(retry.Write) != 0 {
						t.Fatal("live opened codec retried its original injection")
					}
				} else if len(retry.Write) != 1 || !bytes.Equal(retry.Write[0], injected.Write[0]) {
					t.Fatal("reconstructed codec did not retry the exact trusted request")
				}
				if len(c.Tick(time.Now()).Write) != 0 || c.ready || c.running {
					t.Fatal("retry repeated or bypassed the instructions gate")
				}
				ready := c.Decode(agent.Output, instructionAck(t, id))
				if !c.ready || len(ready.Events) != 1 || ready.Events[0].Kind != agent.SessionReady || len(ready.Write) != 1 || parse(t, ready.Write[0]).Method != "turn/start" {
					t.Fatal("actual injection acknowledgement did not release queued turn", ready)
				}
				if len(c.Tick(time.Now()).Write) != 0 {
					t.Fatal("acknowledged gate replayed an injection or queued input")
				}
			}
		}
	}
	// Mutate only the envelope of the real acknowledgement, and replay an
	// actual CLI failure with its request id correlated to this gate.
	var rejected [][]byte
	for _, result := range []any{nil, json.RawMessage(limits)} {
		var ack map[string]any
		json.Unmarshal(instructionAck(t, 4), &ack)
		ack["result"] = result
		b, _ := json.Marshal(ack)
		rejected = append(rejected, b)
	}
	for _, line := range replay(t, "resume_unknown") {
		var response map[string]any
		json.Unmarshal(line.Msg, &response)
		if response["error"] != nil {
			response["id"] = 4
			b, _ := json.Marshal(response)
			rejected = append(rejected, b)
			break
		}
	}
	if len(rejected) != 3 {
		t.Fatal("missing captured failure")
	}
	for _, bad := range rejected {
		c := NewCodec(agent.Spec{Confine: &agent.Confinement{}}, Options{confinedInstructions: "current-composed-instructions"}).(*codec)
		c.Decode(agent.Output, thread)
		c.Input(agent.Input{ID: "queued", Text: "literal"})
		a := c.Decode(agent.Output, bad)
		if !c.closed || c.ready || len(a.Write) != 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.BlockedAtStart {
			t.Fatal("invalid instruction acknowledgement released startup", a)
		}
		if len(c.Tick(time.Now()).Write) != 0 {
			t.Fatal("recovery retried a refused instruction gate")
		}
	}
	// An unopened replay must refuse the same missing trusted state as Open.
	missing := NewCodec(agent.Spec{Confine: &agent.Confinement{}}, Options{})
	refused := missing.Decode(agent.Output, thread)
	if len(refused.Write) != 0 || len(refused.Events) != 1 || refused.Events[0].Reason != agent.BlockedAtStart || len(missing.Tick(time.Now()).Write) != 0 {
		t.Fatal("recovery injected missing trusted instructions")
	}
}

func TestConfinedRepeatedInjectionAcknowledgements(t *testing.T) {
	var thread []byte
	for _, line := range replay(t, "start") {
		if line.Dir == "recv" && parse(t, line.Msg).Result.Thread != nil {
			thread = line.Msg
			break
		}
	}
	acks, err := os.ReadFile("testdata/0.163.0/supervisor_injection_retry/acks.jsonl")
	if err != nil || thread == nil {
		t.Fatal("missing real repeated-injection capture", err)
	}
	c := NewCodec(agent.Spec{Confine: &agent.Confinement{}}, Options{confinedInstructions: "current-recovery-instructions"})
	c.Decode(agent.Output, thread)
	c.Input(agent.Input{ID: "queued", Text: "one-input"})
	if len(c.Tick(time.Now()).Write) != 1 {
		t.Fatal("pending replay did not retry")
	}
	ready, turns, count := 0, 0, 0
	for _, ack := range bytes.Split(bytes.TrimSpace(acks), []byte("\n")) {
		count++
		a := c.Decode(agent.Output, ack)
		for _, e := range a.Events {
			if e.Kind == agent.SessionReady {
				ready++
			}
		}
		for _, write := range a.Write {
			if parse(t, write).Method != "turn/start" {
				t.Fatal("unexpected action after injection acknowledgement")
			}
			turns++
		}
	}
	if count != 2 || ready != 1 || turns != 1 || len(c.Tick(time.Now()).Write) != 0 {
		t.Fatal("real repeated acknowledgements duplicated readiness or queued input")
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
	var catalog struct {
		Models []struct {
			Slug string `json:"slug"`
			Mode string `json:"tool_mode"`
		} `json:"models"`
	}
	if err := json.Unmarshal(launch.Files[confinedCatalogFile], &catalog); err != nil || len(catalog.Models) != 1 || catalog.Models[0].Slug != confinedModel || catalog.Models[0].Mode != "direct" || !strings.Contains(config, "model_catalog_json=") {
		t.Fatal("launch could retain unsafe code-mode executors", err)
	}
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
	r.c.(*codec).opts.confinedInstructions = confinedInputNote
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
		replayed := a.NewCodec(spec).Decode(agent.Output, instructionAck(t, 4))
		if len(replayed.Events) != 1 || replayed.Events[0].Reason != agent.BlockedAtStart {
			t.Fatal("invalid instructions accepted during reconnect", replayed)
		}
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
