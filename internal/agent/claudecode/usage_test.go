package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// The usage capture: two turns, the first of two model messages (a tool
// call between them, each message written as two transcript lines), the
// second of one.
const usageDir = "testdata/2.1.327/usage"

func usageHook(t *testing.T, c *codec, n int) Actions {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(usageDir, "hooks", "*.json"))
	b, err := os.ReadFile(files[n-1])
	if err != nil {
		t.Fatal(err)
	}
	return c.Decode(agent.Hook, envelope("u"+filepath.Base(files[n-1]), files[n-1], b))
}

// usageTranscript returns the capture's transcript lines, split before
// the user line of the second prompt.
func usageTranscript(t *testing.T) (first, second [][]byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(usageDir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	h, _ := os.ReadFile(filepath.Join(usageDir, "hooks", "08-UserPromptSubmit.json"))
	json.Unmarshal(h, &p)
	all := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i, l := range all {
		var tl transcriptLine
		json.Unmarshal(l, &tl)
		if tl.Type == "user" && tl.PromptID == p.PromptID {
			return all[:i], all[i:]
		}
	}
	t.Fatal("no user line of the second prompt")
	return nil, nil
}

func feedLines(c *codec, ls [][]byte) Actions {
	var a Actions
	for _, l := range ls {
		a = merge(a, c.Decode(agent.File, l))
	}
	return a
}

// turnUsage returns, for each turn, the last Usage event before its end,
// and every Usage event.
func turnUsage(t *testing.T, a Actions) (last, all []agent.UsageCount) {
	t.Helper()
	var cur *agent.UsageCount
	for _, e := range a.Events {
		switch e.Kind {
		case agent.TurnStarted:
			cur = nil
		case agent.Usage:
			if e.Usage == nil {
				t.Fatal("a Usage event without counts")
			}
			cur = e.Usage
			all = append(all, *e.Usage)
		case agent.TurnEnded:
			if cur == nil {
				t.Fatal("a turn ended with no usage reported")
			}
			last = append(last, *cur)
		}
	}
	return last, all
}

// The sums of the capture's usage per turn, by hand from
// testdata/2.1.327/usage/transcript.jsonl: the first turn's two messages
// (2+2 input, 145+3 output, 27435+36046 cache reads, 8611+227 cache
// writes) and the second turn's one.
var wantTurns = []agent.UsageCount{
	{Model: "claude-haiku-5-5", Input: 4, Output: 148, CacheRead: 63481, CacheWrite: 8838},
	{Model: "claude-haiku-5-5", Input: 2, Output: 4, CacheRead: 36273, CacheWrite: 40},
}

func checkTurns(t *testing.T, how string, a Actions) {
	t.Helper()
	last, all := turnUsage(t, a)
	if len(last) != len(wantTurns) {
		t.Fatalf("%s: %d turns ended, want %d", how, len(last), len(wantTurns))
	}
	for i, u := range last {
		if u != wantTurns[i] {
			t.Errorf("%s: turn %d: %+v, want %+v", how, i+1, u, wantTurns[i])
		}
	}
	for _, u := range all {
		// The capture gives no context window: context use stays absent.
		if u.ContextUsed != 0 || u.ContextSize != 0 {
			t.Errorf("%s: context use %d of %d, want none", how, u.ContextUsed, u.ContextSize)
		}
	}
	for _, e := range a.Events {
		if e.Kind == agent.Unknown {
			t.Errorf("%s: unknown event %q", how, e.Text)
		}
	}
}

func TestUsageSumsEachTurn(t *testing.T) {
	first, second := usageTranscript(t)

	// As the CLI writes them: each turn's transcript lines before its Stop.
	c := newTestCodec()
	var a Actions
	for n := 1; n <= 2; n++ {
		a = merge(a, usageHook(t, c, n))
	}
	a = merge(a, feedLines(c, first))
	for n := 3; n <= 8; n++ {
		a = merge(a, usageHook(t, c, n))
	}
	a = merge(a, feedLines(c, second))
	for n := 9; n <= 10; n++ {
		a = merge(a, usageHook(t, c, n))
	}
	checkTurns(t, "in order", a)
	// The first message is written twice with the same usage: the totals
	// go up once per message, not per line.
	if _, all := turnUsage(t, a); len(all) != 3 {
		t.Errorf("%d Usage events, want 3 (one per message): %+v", len(all), all)
	}

	// Each turn's transcript read before any hook named its prompt.
	c = newTestCodec()
	a = merge(usageHook(t, c, 1), feedLines(c, first))
	for n := 2; n <= 7; n++ {
		a = merge(a, usageHook(t, c, n))
	}
	a = merge(a, feedLines(c, second))
	for n := 8; n <= 10; n++ {
		a = merge(a, usageHook(t, c, n))
	}
	checkTurns(t, "transcript first", a)
}

// A turn's lines read only after its Stop, before the next turn starts,
// are reported Late, with that turn's totals, so that the core counts them
// toward the ended turn (#675). Read once the next turn runs, or after it
// ended, they are not reported: the core would count them toward that
// turn.
func TestUsageAfterTheTurnEnded(t *testing.T) {
	first, _ := usageTranscript(t)
	c := newTestCodec()
	for n := 1; n <= 7; n++ {
		usageHook(t, c, n)
	}
	var late []agent.UsageCount
	for _, e := range feedLines(c, first).Events {
		if e.Kind == agent.Usage {
			if !e.Late {
				t.Fatalf("an ended turn's usage was reported as a running turn's: %+v", *e.Usage)
			}
			late = append(late, *e.Usage)
		}
	}
	if len(late) == 0 || late[len(late)-1] != wantTurns[0] {
		t.Fatalf("the ended turn's usage: %+v, want it to end at %+v", late, wantTurns[0])
	}
	for _, hooks := range []int{8, 10} {
		c := newTestCodec()
		for n := 1; n <= hooks; n++ {
			usageHook(t, c, n)
		}
		if _, all := turnUsage(t, feedLines(c, first)); len(all) != 0 {
			t.Fatalf("after %d hook calls, an ended turn's usage was reported: %+v", hooks, all)
		}
	}
}

// A transcript line read again, after a restart's re-read, adds nothing.
func TestUsageLineReadTwice(t *testing.T) {
	first, _ := usageTranscript(t)
	c := newTestCodec()
	usageHook(t, c, 1)
	usageHook(t, c, 2)
	feedLines(c, first)
	if _, all := turnUsage(t, feedLines(c, first)); len(all) != 0 {
		t.Fatalf("lines read again gave usage %+v", all)
	}
}

// Usage of a shape the codec does not know is skipped, reported as one
// Unknown event however often it comes, and the turn still ends.
func TestUnknownUsageShape(t *testing.T) {
	// The first turn's real lines, with the usage of each assistant line
	// replaced by a shape the codec does not know.
	first, _ := usageTranscript(t)
	odd := []string{`"many"`, `{"input_tokens":"2","output_tokens":3}`, `{"input":2,"output":3}`}
	var lines [][]byte
	n := 0
	for _, l := range first {
		var tl transcriptLine
		json.Unmarshal(l, &tl)
		if tl.Type == "assistant" {
			var m map[string]any
			json.Unmarshal(l, &m)
			m["message"].(map[string]any)["usage"] = json.RawMessage(odd[n%len(odd)])
			l, _ = json.Marshal(m)
			n++
		}
		lines = append(lines, l)
	}
	if n != 3 {
		t.Fatalf("%d assistant lines in the capture, want 3", n)
	}
	c := newTestCodec()
	a := merge(usageHook(t, c, 1), usageHook(t, c, 2))
	a = merge(a, feedLines(c, lines))
	for n := 3; n <= 7; n++ {
		a = merge(a, usageHook(t, c, n))
	}
	var unknown, usage, ended int
	for _, e := range a.Events {
		switch e.Kind {
		case agent.Unknown:
			unknown++
			if !strings.Contains(e.Text, "usage") {
				t.Errorf("unknown event %q does not say it is usage", e.Text)
			}
		case agent.Usage:
			usage++
		case agent.TurnEnded:
			if e.Status != agent.Completed {
				t.Errorf("turn ended %v", e.Status)
			}
			ended++
		}
	}
	if unknown != 1 || usage != 0 || ended != 1 {
		t.Fatalf("%d unknown, %d usage, %d turns ended; want 1, 0, 1", unknown, usage, ended)
	}
	// The final answer is still the one Stop reports.
	if got := texts(a); len(got) == 0 || got[len(got)-1] != (textEv{"done", true}) {
		t.Fatalf("texts %+v", got)
	}
}

// The line the CLI writes for an API error, with the model "<synthetic>"
// and zero counts, is no model call: it reports no usage.
func TestSyntheticLineGivesNoUsage(t *testing.T) {
	for _, first := range []bool{false, true} {
		a, _ := run(t, newTestCodec(), "failure", first)
		for _, e := range a.Events {
			if e.Kind == agent.Usage || e.Kind == agent.Unknown {
				t.Fatalf("transcript first=%v: %v event %+v", first, e.Kind, e)
			}
		}
	}
}

// The usage capture of Claude Code 2.1.330 (#675): the same two turns, a
// turn with tool calls and a turn that answers with text only, with the
// number of transcript lines each hook call saw (NN-<event>.lines). That
// CLI writes a turn's last message to its transcript after the turn's
// Stop hook: the Stop of the text-only turn saw 53 lines, and its answer
// is line 55.
const usage330Dir = "testdata/2.1.330/usage"

// replay330 gives the codec the capture as the runner reads it, in steps,
// each of which reads the transcript before the hook calls: steps names
// the hook calls of each step, by number, and a step gives the transcript
// lines that its last call saw and that were not given yet, then its
// calls. The rest of the transcript comes last.
func replay330(t *testing.T, c *codec, steps [][]int) Actions {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(usage330Dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	all := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	files, _ := filepath.Glob(filepath.Join(usage330Dir, "hooks", "*.json"))
	if len(files) != 10 {
		t.Fatalf("%d hook calls in the capture, want 10", len(files))
	}
	var a Actions
	given := 0
	for _, step := range steps {
		f := files[step[len(step)-1]-1]
		n, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".lines")
		if err != nil {
			t.Fatal(err)
		}
		seen, err := strconv.Atoi(strings.TrimSpace(string(n)))
		if err != nil {
			t.Fatal(err)
		}
		for ; given < min(seen, len(all)); given++ {
			a = merge(a, c.Decode(agent.File, all[given]))
		}
		for _, h := range step {
			raw, err := os.ReadFile(files[h-1])
			if err != nil {
				t.Fatal(err)
			}
			a = merge(a, c.Decode(agent.Hook, envelope("v"+filepath.Base(files[h-1]), files[h-1], raw)))
		}
	}
	return merge(a, feedLines(c, all[given:]))
}

// usageReport is one Usage event of a turn: the counts, and whether it
// came after the turn's end, Late.
type usageReport struct {
	u    agent.UsageCount
	late bool
}

// reportsOf returns the Usage events of each turn of a, in order, and
// checks that each is Late exactly when it comes after its turn's end.
func reportsOf(t *testing.T, a Actions) [][]usageReport {
	t.Helper()
	var turns [][]usageReport
	ended := 0
	for _, e := range a.Events {
		switch e.Kind {
		case agent.TurnStarted:
			turns = append(turns, nil)
		case agent.TurnEnded:
			ended++
		case agent.Usage:
			if len(turns) == 0 || e.Usage == nil {
				t.Fatalf("a usage event outside a turn: %+v", e)
			}
			if e.Late != (ended == len(turns)) {
				t.Errorf("turn %d: a usage event %+v with Late %v, %d turns ended", len(turns), *e.Usage, e.Late, ended)
			}
			turns[len(turns)-1] = append(turns[len(turns)-1], usageReport{*e.Usage, e.Late})
		case agent.Unknown:
			t.Errorf("unknown event %q", e.Text)
		}
	}
	if ended != 2 || len(turns) != 2 {
		t.Fatalf("%d turns started, %d ended; want 2 and 2", len(turns), ended)
	}
	return turns
}

// The usage of the capture's messages: the tool calls' message, written as
// two lines; the first turn's answer; the text-only turn's one message.
var (
	usage330Tools  = agent.UsageCount{Model: "claude-haiku-5-5", Input: 2, Output: 145, CacheRead: 30220, CacheWrite: 5535}
	usage330First  = agent.UsageCount{Model: "claude-haiku-5-5", Input: 4, Output: 148, CacheRead: 65975, CacheWrite: 5762}
	usage330Second = agent.UsageCount{Model: "claude-haiku-5-5", Input: 2, Output: 4, CacheRead: 35982, CacheWrite: 40}
)

// A turn that calls no tool, whose last message Claude Code wrote after
// its Stop, gives a usage event with its totals: Late, so that the core
// counts it toward that turn. A turn with tool calls gives its first
// message's usage before its end, and its totals, each message once, after
// it (#675). A step that reads a Stop with the hook call after it, by
// when the line is written, reads the line first: the same totals then
// come before the end.
func TestUsageOfAMessageWrittenAfterTheStop(t *testing.T) {
	var each [][]int
	for h := 1; h <= 10; h++ {
		each = append(each, []int{h})
	}
	got := reportsOf(t, replay330(t, newTestCodec(), each))
	want := [][]usageReport{
		{{usage330Tools, false}, {usage330First, true}},
		{{usage330Second, true}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("each hook call in a step of its own:\n got %+v\nwant %+v", got, want)
	}

	together := [][]int{{1}, {2}, {3}, {4}, {5}, {6}, {7, 8}, {9, 10}}
	got = reportsOf(t, replay330(t, newTestCodec(), together))
	want = [][]usageReport{
		{{usage330Tools, false}, {usage330First, false}},
		{{usage330Second, false}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("each Stop read with the call after it:\n got %+v\nwant %+v", got, want)
	}
}
