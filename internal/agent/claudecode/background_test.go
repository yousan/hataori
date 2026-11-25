package claudecode

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

const (
	failedTurn  = "failure_background"
	stoppedTurn = "interrupt_background"
)

// background331 gives c the capture's hook calls whose number skip does not
// hold, each after the transcript lines it saw, then the rest of the
// transcript, and returns what c produced.
func background331(t *testing.T, c *codec, scenario string, skip ...int) []agent.Event {
	t.Helper()
	dir := filepath.Join("testdata/2.1.331", scenario)
	hooks, err := filepath.Glob(filepath.Join(dir, "hooks/*.json"))
	if err != nil || len(hooks) == 0 {
		t.Fatalf("no hooks of %s: %v", scenario, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	var out []agent.Event
	read := func(n int) {
		for n > 0 && len(lines) > 0 {
			out = append(out, c.Decode(agent.File, lines[0]).Events...)
			lines, n = lines[1:], n-1
		}
	}
	given := 0
	for i, f := range hooks {
		l, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".lines")
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := strconv.Atoi(strings.TrimSpace(string(l))); n > given {
			read(n - given)
			given = n
		}
		if slices.Contains(skip, i+1) {
			continue
		}
		p, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c.Decode(agent.Hook, envelope(strconv.Itoa(i+1), f, p)).Events...)
	}
	read(len(lines))
	return out
}

// ends lists the turns' ends of evs.
func ends(evs []agent.Event) string {
	var out []string
	for _, e := range evs {
		if e.Kind == agent.TurnEnded {
			how := map[agent.EndStatus]string{agent.Completed: "completed", agent.Failed: "failed", agent.Interrupted: "interrupted"}[e.Status]
			out = append(out, fmt.Sprintf("end(%s,bg=%d)", how, e.Background))
		}
	}
	return strings.Join(out, " ")
}

// A failed turn, whose StopFailure has no list of background tasks, and an
// interrupted one end with the count of those that still run: what the last
// list said, what was started since, less what ended since (#801).
func TestATurnThatDoesNotCompleteCountsItsBackgroundTasks(t *testing.T) {
	for _, tc := range []struct {
		name, scenario string
		skip           []int
		want           string
	}{
		{"failed", failedTurn, nil,
			"end(completed,bg=2) end(failed,bg=2) end(completed,bg=1) end(completed,bg=0)"},
		{"interrupted", stoppedTurn, nil,
			"end(completed,bg=2) end(interrupted,bg=2) end(completed,bg=1) end(completed,bg=0)"},
		// The turn that starts them fails: no list ever named them.
		{"started in the failed turn", failedTurn, []int{11, 12},
			"end(failed,bg=2) end(completed,bg=1) end(completed,bg=0)"},
		// The result of the tool that started the shell was not seen.
		{"a start missed", failedTurn, []int{8, 11, 12},
			"end(failed,bg=1) end(completed,bg=1) end(completed,bg=0)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ends(background331(t, newTestCodec(), tc.scenario, tc.skip...)); got != tc.want {
				t.Errorf("ends\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// What ended between the last list and the failure is not counted: a
// subagent by its SubagentStop, a shell by its notification, be it the
// prompt of a turn of the agent's own or the transcript's line of it.
func TestAFailedTurnDoesNotCountWhatEnded(t *testing.T) {
	dir := "../2.1.331/" + failedTurn + "/hooks/"
	line := func(t *testing.T, id string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata/2.1.331", failedTurn, "transcript.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range bytes.Split(b, []byte("\n")) {
			if bytes.Contains(l, []byte(`"operation":"enqueue"`)) && bytes.Contains(l, []byte("<task-id>"+id)) {
				return l
			}
		}
		t.Fatalf("no enqueue line for %s", id)
		return nil
	}
	const subagent, shell = "ac9655e008ce5aebe", "b0bgb58y9"
	for name, tc := range map[string]struct {
		then func(t *testing.T, c *codec)
		want string
	}{
		"nothing ended": {func(t *testing.T, c *codec) {}, "end(failed,bg=2)"},
		"SubagentStop": {func(t *testing.T, c *codec) {
			decodeFile(t, c, "15", dir+"15-SubagentStop.json")
		}, "end(failed,bg=1)"},
		"the notification's prompt": {func(t *testing.T, c *codec) {
			decodeFile(t, c, "18", dir+"18-UserPromptSubmit.json")
		}, "end(failed,bg=1)"},
		"the transcript's lines": {func(t *testing.T, c *codec) {
			c.Decode(agent.File, line(t, subagent))
			c.Decode(agent.File, line(t, shell))
		}, "end(failed,bg=0)"},
		// A task whose start is read after its end does not run.
		"the start after the end": {func(t *testing.T, c *codec) {
			c.Decode(agent.File, line(t, shell))
			decodeFile(t, c, "08", dir+"08-PostToolUse.json")
		}, "end(failed,bg=1)"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestCodec()
			decodeFile(t, c, "01", dir+"01-SessionStart.json")
			decodeFile(t, c, "11", dir+"11-Stop.json")
			tc.then(t, c)
			decodeFile(t, c, "12", dir+"12-UserPromptSubmit.json")
			if got := ends(decodeFile(t, c, "13", dir+"13-StopFailure.json").Events); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}
