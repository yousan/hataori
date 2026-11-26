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
// captures of the texts as they are show what would have been: --restricted
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
	for _, name := range []string{"mention_restricted_pasted", "mention_unrestricted_pasted"} {
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
			// The prompt the CLI reports is known as hataori's input, also
			// by the codec after a restart. The CLI takes characters that do
			// not show out of a paste, so that one is not known (#916); an
			// escape sequence is not pasted (#914).
			known := mc.Case != "after_invisible"
			if by, _ := c.starter(mc.Prompt); (by == agent.ByCaller) != known {
				t.Errorf("%s: the prompt %q counts as started by %v", id, mc.Prompt, by)
			}
			c = confinedCodec(t)
			if a, err := c.Input(agent.Input{ID: "in-1", Text: mc.Text, Written: true}); err != nil || len(a.Keys) != 0 {
				t.Fatalf("%s: after a restart: writes %q, %v", id, a.Keys, err)
			}
			if by, _ := c.starter(mc.Prompt); (by == agent.ByCaller) != known {
				t.Errorf("%s: after a restart: the prompt %q counts as started by %v", id, mc.Prompt, by)
			}
		}
		for what := range took {
			if seen[what] != 2 {
				t.Errorf("%s: %s was pasted %d times, want it at a start and at a resume", name, what, seen[what])
			}
		}
	}
}

// An input that a hataori before #887 pasted, its "@" as they are, or one
// before #914, and that the agent reports after a restart to this one, is
// known as hataori's, and so is one with an escape sequence, which this one
// holds (#914), by what the CLI took out of it; one with a character that
// does not show is not (#916). An input that this one pasted since its start
// is not known by such a prompt: a person typed that.
func TestInputPastedWithItsAtSignIsKnown(t *testing.T) {
	for _, mc := range mentionCases(t, "mention_restricted") {
		c := confinedCodec(t)
		if _, err := c.Input(agent.Input{ID: "in-1", Text: mc.Text, Written: true, Earlier: true}); err != nil {
			t.Fatal(err)
		}
		by, id := c.starter(mc.Prompt)
		if known := mc.Case != "after_invisible"; (by == agent.ByCaller && id == "in-1" && c.sent == nil) != known {
			t.Errorf("%s: after a restart: the prompt %q counts as started by %v, input %q", mc.Case, mc.Prompt, by, id)
		}
		if !strings.Contains(mc.Prompt, "@") {
			continue
		}
		c = confinedCodec(t)
		mentionPaste(t, c, mc.Text)
		if by, _ := c.starter(mc.Prompt); by != agent.ByTerminal || c.sent == nil {
			t.Errorf("%s: the prompt %q, which was not pasted, counts as started by %v", mc.Case, mc.Prompt, by)
		}
	}
	// One that a hataori from #887 to #914 pasted, with "(at)" for each "@"
	// and with control characters, is known too.
	for _, name := range []string{"mention_restricted_written", "mention_unrestricted_written"} {
		for _, mc := range mentionCases(t, name) {
			c := confinedCodec(t)
			if _, err := c.Input(agent.Input{ID: "in-1", Text: mc.Text, Written: true, Earlier: true}); err != nil {
				t.Fatal(err)
			}
			by, id := c.starter(mc.Prompt)
			if known := mc.Case != "after_invisible"; (by == agent.ByCaller && id == "in-1" && c.sent == nil) != known {
				t.Errorf("%s %s %s: after a restart: the prompt %q counts as started by %v, input %q", name, mc.Session, mc.Case, mc.Prompt, by, id)
			}
		}
	}
	// One that would be a command was pasted after the prefix before, too.
	c := confinedCodec(t)
	if _, err := c.Input(agent.Input{ID: "in-1", Text: "/add-dir @x", Written: true, Earlier: true}); err != nil {
		t.Fatal(err)
	}
	if by, id := c.starter(commandPrefix + "/add-dir @x"); by != agent.ByCaller || id != "in-1" {
		t.Errorf("after a restart: a command with an at sign counts as started by %v, input %q", by, id)
	}
}

// What the CLI reports of a paste is the paste without the escape sequences
// that start with ESC [ and without the characters that do not show (shown),
// in a person's message, in a long one and in a wake's or a reminder's.
func TestPromptShowsThePaste(t *testing.T) {
	for _, name := range []string{"mention_ordinary", "mention_restricted", "mention_restricted_pasted", "mention_restricted_written",
		"mention_unrestricted", "mention_unrestricted_pasted", "mention_unrestricted_written"} {
		for _, mc := range mentionCases(t, name) {
			if got, want := normalize(shown(mc.Pasted)), normalize(unwrapPaste(mc.Prompt)); got != want {
				t.Errorf("%s %s %s: %q shows as %q; the CLI reported %q", name, mc.Session, mc.Case, mc.Pasted, got, want)
			}
		}
	}
}

// An input that may be in the box after a restart is not known by what the
// CLI reports of a paste that no hataori made for it: one with a character
// that does not show at its start, by the prompt of the same text without
// it, as the CLI reports hataori's paste of either; and one that an older
// hataori pasted as it is, by the prompt of another input that is this
// hataori's paste for it, which an older one pasted as it is too (#914).
func TestHeldInputIsNotKnownByAnotherPaste(t *testing.T) {
	n := 0
	for _, name := range []string{"mention_restricted_pasted", "mention_unrestricted_pasted"} {
		for _, mc := range mentionCases(t, name) {
			if mc.Case != "first" {
				continue
			}
			n++
			c := confinedCodec(t)
			if _, err := c.Input(agent.Input{ID: "in-1", Text: "\u200b" + mc.Text, Written: true, Earlier: true}); err != nil || len(c.leftover) != 1 {
				t.Fatalf("%s %s: held %q, %v", name, mc.Session, c.leftover, err)
			}
			if by, id := c.starter(mc.Prompt); by == agent.ByCaller {
				t.Errorf("%s %s: the prompt %q counts as input %q", name, mc.Session, mc.Prompt, id)
			}
			c = confinedCodec(t)
			for _, in := range []agent.Input{{ID: "in-1", Text: mc.Text}, {ID: "in-2", Text: mc.Pasted}} {
				in.Written, in.Earlier = true, true
				if _, err := c.Input(in); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(c.leftover, []string{"in-1"}) {
				t.Fatalf("%s %s: held %q", name, mc.Session, c.leftover)
			}
			if by, id := c.starter(mc.Prompt); by != agent.ByCaller || id != "in-2" || !slices.Equal(c.leftover, []string{"in-1"}) {
				t.Errorf("%s %s: the prompt %q counts as started by %v, input %q; held %q", name, mc.Session, mc.Prompt, by, id, c.leftover)
			}
		}
	}
	if n == 0 {
		t.Fatal("no capture of a text pasted after the prefix")
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
		c := started(t)
		if got := mentionPaste(t, c, mc.Text); got != mc.Text {
			t.Errorf("%s: %q is pasted as %q", mc.Case, mc.Text, got)
		}
		restarted := started(t)
		if _, err := restarted.Input(agent.Input{ID: "in-1", Text: mc.Text, Written: true}); err != nil {
			t.Fatal(err)
		}
		known := !slices.Contains([]string{"after_escape", "after_invisible"}, mc.Case) // #916
		for what, c := range map[string]*codec{"": c, "after a restart: ": restarted} {
			if by, _ := c.starter(mc.Prompt); (by == agent.ByCaller) != known {
				t.Errorf("%s: %sthe prompt %q counts as started by %v", mc.Case, what, mc.Prompt, by)
			}
		}
	}
}
