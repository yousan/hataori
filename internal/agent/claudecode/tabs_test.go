package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// pasteTabs is the capture of pasted input with tabs (#393): the CLI's input
// box reports each tab as four spaces.
const pasteTabs = "testdata/2.1.328/paste_tabs"

func readPasteTabs(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pasteTabs, file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// startedPasteTabs returns a codec past the capture's SessionStart.
func startedPasteTabs(t *testing.T) *codec {
	c := newTestCodec()
	c.Decode(agent.Hook, envelope("start", "", readPasteTabs(t, "hooks/01-SessionStart.json")))
	return c
}

// A pasted input with tabs is the caller's turn, with its id, and no
// "not delivered" failure follows.
func TestPastedTabsAreDelivered(t *testing.T) {
	for _, pair := range []struct{ pasted, hook string }{
		{"pasted-1.txt", "hooks/02-UserPromptSubmit.json"},
		{"pasted-2.txt", "hooks/04-UserPromptSubmit.json"},
	} {
		c := startedPasteTabs(t)
		if a, _ := c.Input(agent.Input{ID: "in-9", Text: string(readPasteTabs(t, pair.pasted))}); len(a.Keys) != 1 {
			t.Fatalf("%s: not pasted: %q", pair.pasted, a.Keys)
		}
		now := time.Now()
		c.Tick(now)
		c.Tick(now.Add(pasteSettle)) // Enter
		evs := c.Decode(agent.Hook, envelope("u", "", readPasteTabs(t, pair.hook)))
		if got := summary(evs); got != "turn(caller)" {
			t.Fatalf("%s: %s, want the caller's turn", pair.hook, got)
		}
		if evs.Events[0].Input != "in-9" {
			t.Fatalf("%s: the turn reports input %q, want in-9", pair.hook, evs.Events[0].Input)
		}
		if a := c.Tick(now.Add(time.Hour)); len(a.Events) != 0 {
			t.Fatalf("%s: delivered input failed later: %+v", pair.hook, a.Events)
		}
	}
}

// Only a tab against four spaces, trimming and line endings are forgiven
// when an input is matched to its prompt.
func TestPromptMatchForgivesOnlyTabsTrimAndLineEndings(t *testing.T) {
	var p map[string]any
	json.Unmarshal(readPasteTabs(t, "hooks/02-UserPromptSubmit.json"), &p)
	pasted := string(readPasteTabs(t, "pasted-1.txt")) // "tab\there, two\t\ttabs; end"
	for _, tc := range []struct {
		input, prompt string
		match         bool
	}{
		{pasted, p["prompt"].(string), true},
		{pasted, "tab\there, two\t\ttabs; end", true},     // a CLI that keeps tabs
		{pasted, "tab    here, two\t    tabs; end", true}, // either per tab
		{"\r\n " + pasted + "\t\r\n", "tab    here, two        tabs; end", true},
		{"a\r\n\tb", "a\n    b", true},
		{pasted, "tab   here, two        tabs; end", false},   // three spaces
		{pasted, "tab     here, two        tabs; end", false}, // five spaces
		{pasted, "tab  here, two    tabs; end", false},        // two per tab
		{pasted, "tab here, two  tabs; end", false},           // one per tab
		{pasted, "tabhere, twotabs; end", false},              // dropped
		{pasted, "tab\nhere, two\n\ntabs; end", false},        // a newline per tab
		{pasted, "tab    here, two        tabs; End", false},  // another letter
		{pasted, "tab    here, two        tabs; end.", false},
		{"a  b", "a b", false},
		{"a\n\nb", "a\nb", false},
	} {
		c := startedPasteTabs(t)
		c.Input(agent.Input{ID: "in-1", Text: tc.input})
		now := time.Now()
		c.Tick(now)
		c.Tick(now.Add(pasteSettle))
		q := map[string]any{}
		for k, v := range p {
			q[k] = v
		}
		q["prompt"] = tc.prompt
		raw, _ := json.Marshal(q)
		evs := c.Decode(agent.Hook, envelope("u", "", raw))
		want := "turn(terminal)"
		if tc.match {
			want = "turn(caller)"
		}
		if got := summary(evs); got != want {
			t.Errorf("input %q, prompt %q: %s, want %s", tc.input, tc.prompt, got, want)
			continue
		}
		// The turn of a prompt that is not the input ends first.
		end := []byte(`{"hook":"s","wait":false,"payload":{"hook_event_name":"Stop"}}`)
		c.Decode(agent.Hook, end)
		c.Tick(now.Add(time.Minute))
		late := c.Tick(now.Add(time.Hour))
		failed := len(late.Events) == 1 && late.Events[0].Reason == agent.NotDelivered && late.Events[0].Input == "in-1"
		if tc.match == failed {
			t.Errorf("input %q, prompt %q: after the timeout %+v", tc.input, tc.prompt, late.Events)
		}
	}
}
