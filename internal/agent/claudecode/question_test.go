package claudecode

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// replyInput returns the updatedInput of a reply to a question.
func replyInput(t *testing.T, a Actions) map[string]any {
	t.Helper()
	if len(a.Reply) != 1 {
		t.Fatalf("replies: %+v", a.Reply)
	}
	var r struct {
		HookSpecificOutput struct {
			HookEventName      string
			PermissionDecision string
			UpdatedInput       map[string]any
		}
	}
	if err := json.Unmarshal(a.Reply[0].Body, &r); err != nil {
		t.Fatal(err)
	}
	o := r.HookSpecificOutput
	if o.HookEventName != "PreToolUse" || o.PermissionDecision != "allow" {
		t.Fatalf("reply %s", a.Reply[0].Body)
	}
	return o.UpdatedInput
}

// capturedInput is the tool input the CLI ran with after the real answer:
// what PostToolUse reports.
func capturedInput(t *testing.T, file string) map[string]any {
	var p struct {
		ToolInput map[string]any `json:"tool_input"`
	}
	json.Unmarshal(readCapture(t, file), &p)
	return p.ToolInput
}

func TestQuestionBecomesARequest(t *testing.T) {
	c := newTestCodec()
	a := decodeFile(t, c, "q1", "question/hooks/03-PreToolUse.json")
	if len(a.Events) != 1 || a.Events[0].Kind != agent.Request {
		t.Fatalf("events %+v", a.Events)
	}
	r := a.Events[0].Request
	if r.ID != "q1" || !reflect.DeepEqual(r.Choices, []string{"red", "blue"}) || r.Text != "Which colour?\n- red: Choose red\n- blue: Choose blue" {
		t.Fatalf("request %+v", r)
	}
	ans, err := c.Answer(agent.Answer{Request: "q1", Choices: []string{"red"}})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Reply[0].Call != "q1" {
		t.Fatalf("reply to %q", ans.Reply[0].Call)
	}
	// The same input the successful run's PostToolUse shows.
	if got, want := replyInput(t, ans), capturedInput(t, "question/hooks/04-PostToolUse.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reply input\n%v\nwant\n%v", got, want)
	}
}

func TestFreeTextAndSeveralOptions(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		answer   agent.Answer
	}{
		{"question-free", agent.Answer{Text: "green, because it is calm"}},
		{"question-multi", agent.Answer{Choices: []string{"red", "blue", "green"}}},
	} {
		c := newTestCodec()
		decodeFile(t, c, "q1", tc.scenario+"/hooks/03-PreToolUse.json")
		tc.answer.Request = "q1"
		a, err := c.Answer(tc.answer)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := replyInput(t, a), capturedInput(t, tc.scenario+"/hooks/04-PostToolUse.json"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: reply input\n%v\nthe CLI accepted\n%v", tc.scenario, got, want)
		}
	}
}

func TestPermissionAllowedAtOnce(t *testing.T) {
	a := decodeFile(t, newTestCodec(), "p1", "tools/hooks/04-PermissionRequest.json")
	if len(a.Events) != 0 || len(a.Reply) != 1 || a.Reply[0].Call != "p1" {
		t.Fatalf("permission request: %+v", a)
	}
	var r struct {
		HookSpecificOutput struct {
			Decision struct{ Behavior string }
		}
	}
	json.Unmarshal(a.Reply[0].Body, &r)
	if r.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("reply %s", a.Reply[0].Body)
	}
}

func TestAnswerOnlyWhileWaiting(t *testing.T) {
	c := newTestCodec()
	if _, err := c.Answer(agent.Answer{Request: "nope", Text: "x"}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("unknown call: %v", err)
	}
	decodeFile(t, c, "q1", "question/hooks/03-PreToolUse.json")
	if _, err := c.Answer(agent.Answer{Request: "q1", Text: "red"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Answer(agent.Answer{Request: "q1", Text: "blue"}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("second answer: %v", err)
	}
}

// After the hook's timeout the CLI shows its own dialog instead
// (question-timeout-q); the request is withdrawn and cannot be answered.
func TestQuestionWithdrawnAtTheHookTimeout(t *testing.T) {
	c := newTestCodec()
	c.hookTimeout = time.Minute
	decodeFile(t, c, "q1", "question-timeout-q/hooks/03-PreToolUse.json")
	now := time.Now()
	c.Tick(now)
	if a := c.Tick(now.Add(59 * time.Second)); len(a.Events) != 0 {
		t.Fatalf("withdrawn early: %+v", a.Events)
	}
	a := c.Tick(now.Add(time.Minute))
	if len(a.Events) != 1 || a.Events[0].Kind != agent.RequestWithdrawn || a.Events[0].Request.ID != "q1" {
		t.Fatalf("at the timeout: %+v", a.Events)
	}
	if _, err := c.Answer(agent.Answer{Request: "q1", Text: "red"}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("answer after withdrawal: %v", err)
	}
}

// A daemon restart loses the codec. The host still holds the waiting call
// (#50); the new codec learns it from the host's list and answers it.
func TestQuestionAnsweredAfterRestart(t *testing.T) {
	before := newTestCodec()
	decodeFile(t, before, "q1", "question/hooks/03-PreToolUse.json")

	waiting := []host.WaitingCall{{ID: "q1", Payload: readCapture(t, "question/hooks/03-PreToolUse.json")}}
	after := newTestCodec()
	for _, w := range waiting {
		after.Decode(agent.Hook, host.CallLine(w.ID, true, w.Payload))
	}
	a, err := after.Answer(agent.Answer{Request: "q1", Choices: []string{"red"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := replyInput(t, a), capturedInput(t, "question/hooks/04-PostToolUse.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reply after restart: %v", got)
	}
}
