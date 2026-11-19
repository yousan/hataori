package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// A question that takes several options says so; one that takes one does
// not. Both from real captures.
func TestMultiSelectQuestionIsMarked(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		multi    bool
	}{{"question", false}, {"question-multi", true}} {
		a := decodeFile(t, newTestCodec(), "q1", tc.scenario+"/hooks/03-PreToolUse.json")
		if len(a.Events) != 1 || a.Events[0].Request == nil || a.Events[0].Request.Multi != tc.multi {
			t.Errorf("%s: events %+v, want a request with Multi %v", tc.scenario, a.Events, tc.multi)
		}
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

// A daemon restart loses the codec, not the question: the session host
// keeps the hook's call open (#50). A new codec learns the call from the
// host's list of waiting calls and answers it; the hook prints the answer.
func TestQuestionAnsweredAfterRestart(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { killGroups(dir) })
	if _, err := host.Start(host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, dir,
		host.Spec{Argv: []string{os.Args[0], "role", "idle"}, Env: []string{}}); err != nil {
		t.Fatal(err)
	}
	// The agent's hook asks the question and waits.
	hook := exec.Command(os.Args[0], "role", "hook", "--wait", "--timeout", "20s")
	hook.Env = []string{host.HookEnv(dir)}
	hook.Stdin = bytes.NewReader(readCapture(t, "question/hooks/03-PreToolUse.json"))
	var out bytes.Buffer
	hook.Stdout = &out
	if err := hook.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- hook.Wait() }()

	// Before the restart: the codec saw the call in events.ndjson.
	before := newTestCodec()
	deadline := time.Now().Add(10 * time.Second)
	var asked Actions
	for len(asked.Events) == 0 && time.Now().Before(deadline) {
		if b, _ := os.ReadFile(filepath.Join(dir, host.EventsFile)); len(b) > 0 {
			asked = before.Decode(agent.Hook, bytes.TrimSpace(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(asked.Events) != 1 || asked.Events[0].Kind != agent.Request {
		t.Fatalf("before the restart: %+v", asked.Events)
	}

	// After the restart: a new codec, and only the host to ask.
	c, err := host.Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waiting, err := c.Waiting()
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting calls: %+v, %v", waiting, err)
	}
	after := newTestCodec()
	if a := after.Decode(agent.Hook, waiting[0].Line()); len(a.Events) != 1 || a.Events[0].Request.ID != asked.Events[0].Request.ID {
		t.Fatalf("after the restart: %+v", a.Events)
	}
	a, err := after.Answer(agent.Answer{Request: waiting[0].ID, Choices: []string{"red"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Answer(a.Reply[0].Call, a.Reply[0].Body); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || !bytes.Equal(out.Bytes(), a.Reply[0].Body) {
			t.Fatalf("hook printed %q, %v", out.Bytes(), err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook did not get its answer")
	}
}

// The timeout runs from when the host took the call, also for a codec that
// learns of the call later.
func TestTimeoutFromTheCall(t *testing.T) {
	c := newTestCodec()
	c.hookTimeout = time.Minute
	asked := time.Now().Add(-2 * time.Minute)
	c.Decode(agent.Hook, host.CallLine("q1", true, asked, readCapture(t, "question/hooks/03-PreToolUse.json")))
	if a := c.Tick(time.Now()); len(a.Events) != 1 || a.Events[0].Kind != agent.RequestWithdrawn {
		t.Fatalf("an expired call: %+v", a.Events)
	}
}

// A question the CLI went past without an answer is withdrawn first, and
// can no longer be answered.
func TestQuestionWithdrawnWhenTheCLIGoesOn(t *testing.T) {
	for _, file := range []string{"question/hooks/05-Stop.json", "question/hooks/06-SessionEnd.json", "question/hooks/04-PostToolUse.json"} {
		c := newTestCodec()
		decodeFile(t, c, "q1", "question/hooks/03-PreToolUse.json")
		a := decodeFile(t, c, "x", file)
		if len(a.Events) == 0 || a.Events[0].Kind != agent.RequestWithdrawn {
			t.Fatalf("%s: %+v", file, a.Events)
		}
		if _, err := c.Answer(agent.Answer{Request: "q1", Text: "red"}); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("%s: answer afterwards: %v", file, err)
		}
	}
}
