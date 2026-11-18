package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// A turn's lines read only after its Stop, while the next turn runs, are
// not reported: the core would add them to the running turn.
func TestUsageAfterTheTurnEnded(t *testing.T) {
	first, _ := usageTranscript(t)
	c := newTestCodec()
	for n := 1; n <= 8; n++ {
		usageHook(t, c, n)
	}
	if _, all := turnUsage(t, feedLines(c, first)); len(all) != 0 {
		t.Fatalf("an ended turn's usage was reported: %+v", all)
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
