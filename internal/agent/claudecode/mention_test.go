package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// mentionCase is what one text did in a real session (testdata/mention.py).
type mentionCase struct {
	Session, Case string // a start or a resume; what the text names, and how
	Text          string // what hataori is given
	Pasted        string // what was pasted for it
	Prompt        string // what UserPromptSubmit reported
	Attachments   []string
	Content       []string // the dummies whose content was sent to the API since
}

func mentionCases(t *testing.T, name string) []mentionCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/2.1.332", name, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cs []mentionCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	if len(cs) == 0 {
		t.Fatalf("%s holds no case", name)
	}
	return cs
}

// mentionPaste is what c writes to the session for text.
func mentionPaste(t *testing.T, c *codec, text string) string {
	t.Helper()
	a, err := c.Input(agent.Input{ID: "in-1", Text: text})
	if err != nil || len(a.Keys) != 1 {
		t.Fatalf("%q: writes %q, %v", text, a.Keys, err)
	}
	return a.Keys[0].Paste
}

func confinedCodec(t *testing.T) *codec {
	t.Helper()
	c := (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, Confine: &agent.Confinement{}}).(*codec)
	decodeFile(t, c, "start", "plain/hooks/01-SessionStart.json")
	return c
}

// Nothing that a text names is put into a confined session's conversation:
// the adapter pastes every text as the captures' were, and those put nothing
// there, with --restricted and without it, at a start and at a resume, in a
// person's message, in a wake's answer and in a reminder's note (#887). The
// captures of the texts as they are show that each would have: --restricted
// keeps out files outside the directory only.
func TestConfinedInputNamesNothing(t *testing.T) {
	// What names something, and how often: the cases with content, as the
	// CLI took them without --restricted.
	took := map[string]int{}
	for _, c := range mentionCases(t, "mention_unrestricted") {
		took[c.Case] += len(c.Content)
	}
	for _, want := range []string{"absolute_outside", "relative_outside", "home", "quoted_outside", "directory_outside", "link_outside",
		"relative_inside", "absolute_inside", "resource", "long", "wake", "reminder"} {
		if took[want] == 0 {
			t.Errorf("%s: the capture shows nothing that was put into the conversation, so its other form shows nothing", want)
		}
	}
	for _, name := range []string{"mention_restricted_written", "mention_unrestricted_written"} {
		seen := map[string]int{}
		// What the session's first text, which names nothing, brings with it
		// is no mention.
		var first []string
		for _, mc := range mentionCases(t, name) {
			seen[mc.Case]++
			id := name + " " + mc.Session + " " + mc.Case
			c := confinedCodec(t)
			if got := mentionPaste(t, c, mc.Text); got != mc.Pasted {
				t.Errorf("%s: %q is pasted as %q; the capture is of %q", id, mc.Text, got, mc.Pasted)
			}
			if len(mc.Content) != 0 {
				t.Errorf("%s: put into the conversation: %q", id, mc.Content)
			}
			if mc.Case == "none" {
				first = mc.Attachments
			}
			for _, kind := range mc.Attachments {
				if mc.Case != "none" && kind != "total_tokens_reminder" {
					t.Errorf("%s: an attachment %q (the first text's: %q)", id, kind, first)
				}
			}
			// The prompt the CLI reports is known as hataori's input. It
			// takes escape sequences and characters that do not show out of
			// a paste, so those two are not.
			if by, _ := c.starter(mc.Prompt); (by == agent.ByCaller) == slices.Contains([]string{"after_escape", "after_invisible"}, mc.Case) {
				t.Errorf("%s: the prompt %q counts as started by %v", id, mc.Prompt, by)
			}
		}
		for what := range took {
			if seen[what] != 2 {
				t.Errorf("%s: %s was pasted %d times, want it at a start and at a resume", name, what, seen[what])
			}
		}
	}
}

// What --restricted alone does about a text that is pasted as it is.
func TestRestrictedTakesMentionsInsideTheDirectory(t *testing.T) {
	content := map[string][]string{}
	for _, c := range mentionCases(t, "mention_restricted") {
		content[c.Case] = append(content[c.Case], c.Content...)
	}
	for _, what := range []string{"absolute_outside", "relative_outside", "home", "quoted_outside", "directory_outside", "link_outside"} {
		if len(content[what]) != 0 {
			t.Errorf("%s: %q", what, content[what])
		}
	}
	for _, what := range []string{"relative_inside", "absolute_inside", "resource", "long"} {
		if len(content[what]) != 2 {
			t.Errorf("%s: %q, want it at a start and at a resume", what, content[what])
		}
	}
	for _, c := range slices.Concat(content["wake"], content["reminder"]) {
		if strings.Contains(c, "outside") {
			t.Errorf("in a wake: %q", c)
		}
	}
}

// An ordinary session is pasted each text as it is.
func TestOrdinaryInputKeepsTheAtSign(t *testing.T) {
	for _, mc := range mentionCases(t, "mention_ordinary") {
		if mc.Pasted != mc.Text {
			t.Fatalf("%s: the capture is of %q", mc.Case, mc.Pasted)
		}
		if got := mentionPaste(t, started(t), mc.Text); got != mc.Text {
			t.Errorf("%s: %q is pasted as %q", mc.Case, mc.Text, got)
		}
	}
}
