package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func newTestCodec() *codec {
	a := &Adapter{}
	return a.NewCodec(agent.Spec{Approvals: agent.AllowAll}).(*codec)
}

// envelope wraps a captured payload as the session host records a call:
// waiting for the hooks the settings file makes wait.
func envelope(id, file string, raw []byte) []byte {
	var p payload
	json.Unmarshal(raw, &p)
	wait := p.Event == "PermissionRequest" || (p.Event == "PreToolUse" && p.ToolName == askTool)
	head := fmt.Sprintf(`{"hook":%q,"wait":%t,"payload":`, id, wait)
	return append(append([]byte(head), bytes.TrimSpace(raw)...), '}')
}

var kindNames = map[agent.Kind]string{
	agent.Unknown: "unknown", agent.SessionReady: "ready", agent.TurnStarted: "turn", agent.Text: "text",
	agent.ToolStarted: "tool+", agent.ToolEnded: "tool-", agent.SubagentStarted: "sub+", agent.SubagentEnded: "sub-",
	agent.TurnEnded: "end", agent.SessionEnded: "session-end",
}

// describe is a short form of an event, for comparing with expectations.
func describe(e agent.Event) string {
	n := kindNames[e.Kind]
	switch e.Kind {
	case agent.SessionReady:
		return fmt.Sprintf("%s(%t,%s,%s)", n, e.SessionID != "", e.Model, e.Effort)
	case agent.TurnStarted:
		return fmt.Sprintf("%s(%s)", n, [...]string{"caller", "agent", "terminal"}[e.StartedBy])
	case agent.Text:
		return fmt.Sprintf("%s(final=%t)", n, e.Final)
	case agent.TurnEnded:
		if e.Status == agent.Failed {
			return fmt.Sprintf("%s(failed,%s)", n, e.Reason)
		}
		return fmt.Sprintf("%s(completed,bg=%d)", n, e.Background)
	case agent.ToolStarted, agent.ToolEnded, agent.SubagentStarted, agent.SubagentEnded:
		return fmt.Sprintf("%s(%s)", n, e.Tool)
	}
	return n
}

func decodeFile(t *testing.T, c *codec, id, file string) Actions {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/2.1.322", file))
	if err != nil {
		t.Fatal(err)
	}
	return c.Decode(agent.Hook, envelope(id, file, b))
}

func summary(a Actions) string {
	var ks []string
	for _, e := range a.Events {
		ks = append(ks, describe(e))
	}
	if len(a.Reply) > 0 {
		ks = append(ks, "reply")
	}
	return strings.Join(ks, " ")
}

// What each captured payload decodes to, alone in a fresh codec (so every
// prompt counts as typed at the terminal).
var expected = []struct{ file, want string }{
	{"background/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"background/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"background/hooks/03-PreToolUse.json", "tool+(Bash)"},
	{"background/hooks/04-PostToolUse.json", "tool-(Bash)"},
	{"background/hooks/05-Stop.json", "text(final=true) end(completed,bg=1)"},
	{"background/hooks/06-UserPromptSubmit.json", "turn(agent)"},
	{"background/hooks/07-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"background/hooks/08-SessionEnd.json", "session-end"},
	{"compact/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"compact/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"compact/hooks/03-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"compact/hooks/04-PreCompact.json", "tool+(compaction)"},
	{"compact/hooks/05-SubagentStop.json", ""},
	{"compact/hooks/06-SessionStart.json", ""},
	{"compact/hooks/07-PostCompact.json", "tool-(compaction)"},
	{"compact/hooks/08-SessionEnd.json", "session-end"},
	{"failure/hooks/01-SessionStart.json", "ready(true,claude-does-not-exist-0,)"},
	{"failure/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"failure/hooks/03-StopFailure.json", "end(failed,api_error)"},
	{"failure/hooks/04-SessionEnd.json", "session-end"},
	{"interrupt/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"interrupt/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"interrupt/hooks/03-PreToolUse.json", "tool+(Bash)"},
	{"interrupt/hooks/04-PermissionRequest.json", "reply"},
	{"interrupt/hooks/05-SessionEnd.json", "session-end"},
	{"paste/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"paste/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"paste/hooks/03-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"paste/hooks/04-SessionEnd.json", "session-end"},
	{"plain/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"plain/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"plain/hooks/03-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"plain/hooks/04-SessionEnd.json", "session-end"},
	{"question/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"question/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"question/hooks/03-PreToolUse.json", ""},
	{"question/hooks/04-PostToolUse.json", ""},
	{"question/hooks/05-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"question/hooks/06-SessionEnd.json", "session-end"},
	{"resume/hooks/01-SessionStart.json", "ready(true,,)"},
	{"resume/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"resume/hooks/03-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"resume/hooks/04-SessionEnd.json", "session-end"},
	{"subagent/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"subagent/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"subagent/hooks/03-PreToolUse.json", "tool+(Agent)"},
	{"subagent/hooks/04-PostToolUse.json", "tool-(Agent)"},
	{"subagent/hooks/05-SubagentStart.json", "sub+(general-purpose)"},
	{"subagent/hooks/06-SubagentStop.json", "sub-(general-purpose)"},
	{"subagent/hooks/07-Stop.json", "text(final=true) end(completed,bg=1)"},
	{"subagent/hooks/08-UserPromptSubmit.json", "turn(agent)"},
	{"subagent/hooks/09-SessionEnd.json", "session-end"},
	{"tools/hooks/01-SessionStart.json", "ready(true,claude-haiku-5-5,)"},
	{"tools/hooks/02-UserPromptSubmit.json", "turn(terminal)"},
	{"tools/hooks/03-PreToolUse.json", "tool+(Write)"},
	{"tools/hooks/04-PermissionRequest.json", "reply"},
	{"tools/hooks/05-PostToolUse.json", "tool-(Write)"},
	{"tools/hooks/06-Stop.json", "text(final=true) end(completed,bg=0)"},
	{"tools/hooks/07-SessionEnd.json", "session-end"},
}

func TestEachCapture(t *testing.T) {
	files, _ := filepath.Glob("testdata/2.1.322/*/hooks/*.json")
	if len(files) != len(expected) {
		t.Fatalf("%d captures, %d expectations", len(files), len(expected))
	}
	for _, x := range expected {
		t.Run(x.file, func(t *testing.T) {
			a := decodeFile(t, newTestCodec(), "c1", x.file)
			if got := summary(a); got != x.want {
				t.Fatalf("got %q, want %q", got, x.want)
			}
			// The ids come from the payload itself.
			b, _ := os.ReadFile(filepath.Join("testdata/2.1.322", x.file))
			var p payload
			json.Unmarshal(b, &p)
			for _, e := range a.Events {
				switch e.Kind {
				case agent.SessionReady:
					if e.SessionID != p.SessionID || e.Model != p.Model {
						t.Errorf("ready: %+v, payload session %s model %s", e, p.SessionID, p.Model)
					}
				case agent.ToolStarted, agent.ToolEnded:
					if e.Tool != CompactTool && (e.ID == "" || e.ID != p.ToolUseID) {
						t.Errorf("tool id %q, payload %q", e.ID, p.ToolUseID)
					}
				case agent.SubagentStarted, agent.SubagentEnded:
					if e.ID == "" || e.ID != p.AgentID {
						t.Errorf("subagent id %q, payload %q", e.ID, p.AgentID)
					}
				}
			}
			for _, r := range a.Reply {
				if r.Call != "c1" {
					t.Errorf("reply to %q, want the call c1", r.Call)
				}
			}
		})
	}
}

func TestWhoStartedATurn(t *testing.T) {
	raw := func(file string) map[string]any {
		b, _ := os.ReadFile(filepath.Join("testdata/2.1.322", file))
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	// The long paste arrives wrapped in a <pasted_content> tag; the short
	// typed prompt as it is. Both are the caller's once hataori sent them.
	paste := raw("paste/hooks/02-UserPromptSubmit.json")["prompt"].(string)
	inner := strings.TrimSpace(unwrapPaste(paste))
	if inner == paste || !strings.HasPrefix(inner, "-Ignore") || !strings.HasSuffix(inner, "end;") {
		t.Fatalf("unwrapped paste: %.40q ... %.20q", inner, inner[max(0, len(inner)-20):])
	}
	for _, file := range []string{"paste/hooks/02-UserPromptSubmit.json", "plain/hooks/02-UserPromptSubmit.json"} {
		c := newTestCodec()
		c.Input(agent.Input{Text: unwrapPaste(raw(file)["prompt"].(string))})
		if got := summary(decodeFile(t, c, "c1", file)); got != "turn(caller)" {
			t.Errorf("%s after Input: %s", file, got)
		}
		// The same text again, not sent by hataori this time: a person typed it.
		if got := summary(decodeFile(t, c, "c2", file)); got != "turn(terminal)" {
			t.Errorf("%s a second time: %s", file, got)
		}
	}
	if got := summary(decodeFile(t, newTestCodec(), "c1", "background/hooks/06-UserPromptSubmit.json")); got != "turn(agent)" {
		t.Errorf("task notification: %s", got)
	}
}

func TestStopCarriesTheAnswer(t *testing.T) {
	a := decodeFile(t, newTestCodec(), "c1", "background/hooks/05-Stop.json")
	b, _ := os.ReadFile("testdata/2.1.322/background/hooks/05-Stop.json")
	var p payload
	json.Unmarshal(b, &p)
	if a.Events[0].Text != p.LastMessage || p.LastMessage == "" || a.Events[1].Background != 1 {
		t.Fatalf("Stop decoded to %+v", a.Events)
	}
}

// StopFailure's error types. model_not_found is the real capture; the
// others are the same payload with the documented error type, simulated
// until the real CLI produces them.
func TestFailureReasons(t *testing.T) {
	b, _ := os.ReadFile("testdata/2.1.322/failure/hooks/03-StopFailure.json")
	for e, want := range map[string]agent.Reason{
		"model_not_found":       agent.APIError,
		"rate_limit":            agent.Limit,
		"authentication_failed": agent.Login,
		"server_error":          agent.APIError,
	} {
		var m map[string]any
		json.Unmarshal(b, &m)
		m["error"] = e
		p, _ := json.Marshal(m)
		a := newTestCodec().Decode(agent.Hook, envelope("c1", "", p))
		if len(a.Events) != 1 || a.Events[0].Status != agent.Failed || a.Events[0].Reason != want {
			t.Errorf("error %s: %+v", e, a.Events)
		}
	}
}

func TestUnknownAndExtraFields(t *testing.T) {
	c := newTestCodec()
	for _, line := range []string{
		`{"hook":"u1","wait":false,"payload":{"hook_event_name":"SomethingNew","x":1}}`,
		`{"hook":"u2","wait":false,"payload":{"hook_event_name":"SessionEnd","reason":"other","new_field":{"a":[1]}}}`,
		`not json at all`,
		`{"hook":"u3","wait":false,"payload":not json}`,
	} {
		a := c.Decode(agent.Hook, []byte(line))
		if len(a.Events) != 1 {
			t.Errorf("%s: %+v", line, a.Events)
		}
	}
}

func TestSameCallTwice(t *testing.T) {
	c := newTestCodec()
	first := decodeFile(t, c, "c1", "plain/hooks/03-Stop.json")
	again := decodeFile(t, c, "c1", "plain/hooks/03-Stop.json")
	if len(first.Events) != 2 || len(again.Events) != 0 || len(again.Reply) != 0 {
		t.Fatalf("first %d events, again %d", len(first.Events), len(again.Events))
	}
}

// The codec does no I/O: its file imports nothing that reaches outside.
func TestCodecDoesNoIO(t *testing.T) {
	src, err := os.ReadFile("codec.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{`"os"`, `"os/exec"`, `"net"`, `"io"`, `"io/ioutil"`, `"syscall"`} {
		if bytes.Contains(src, []byte(pkg)) {
			t.Errorf("codec.go imports %s", pkg)
		}
	}
}
