package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

type invisibleCase struct {
	Session, Case, Text, Pasted, After string
	FirstPrompts                       []string          `json:"first_prompts"`
	Confirmed                          bool              `json:"confirmed"`
	Prompts                            []string          `json:"prompts"`
	Reports                            []json.RawMessage `json:"reports"`
	FollowingPrompts                   []string          `json:"following_prompts"`
	FollowingReports                   []json.RawMessage `json:"following_reports"`
}

const invisibleDir = "testdata/2.1.332/invisible"

func invisibleCases(t *testing.T, name string) []invisibleCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(invisibleDir, name, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []invisibleCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	want := 39
	if strings.HasPrefix(name, "extra_") {
		want = 12
	}
	if len(cases) != want {
		t.Fatalf("%s: %d cases, want %d", name, len(cases), want)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Case] || !strings.HasSuffix(name, c.Session) || len(c.Prompts) != 1 || len(c.Reports) != 1 ||
			len(c.FollowingPrompts) != 1 || len(c.FollowingReports) != 1 || c.FollowingPrompts[0] != c.After {
			t.Fatalf("%s: repeated/incomplete/combined capture %+v", name, c)
		}
		seen[c.Case] = true
		if c.Confirmed && len(c.FirstPrompts) != 0 || !c.Confirmed && len(c.FirstPrompts) != 1 {
			t.Fatalf("%s/%s: first Enter: %q, confirmed %v", name, c.Case, c.FirstPrompts, c.Confirmed)
		}
	}
	return cases
}

func invisibleCodec(name string, report json.RawMessage) *codec {
	var p payload
	json.Unmarshal(report, &p)
	spec := agent.Spec{TakenIn: true, ResumeID: p.SessionID}
	if strings.Contains(name, "confined") {
		spec.Confine = &agent.Confinement{}
	}
	return (&Adapter{}).NewCodec(spec).(*codec)
}

// Replay actual UserPromptSubmit payloads, both on the current codec and
// after a daemon reconnect with the exact recorded paste. The CLI's first
// Enter/confirmation observations and independent following prompt are
// retained with every case; joining/selector characters in valid text stay
// distinct from a modified prompt.
func TestInvisibleCharacterCaptures(t *testing.T) {
	for _, mode := range []string{"ordinary", "confined", "extra_ordinary", "extra_confined"} {
		for _, phase := range []string{"start", "resume"} {
			name := mode + "_" + phase
			for _, mc := range invisibleCases(t, name) {
				t.Run(name+"/"+mc.Case, func(t *testing.T) {
					if got := normalize(shown(mc.Pasted)); got != normalize(unwrapPaste(mc.Prompts[0])) {
						t.Fatalf("paste %q reports as %q, captured %q", mc.Pasted, got, mc.Prompts[0])
					}
					for _, reconnect := range []bool{false, true} {
						c := invisibleCodec(name, mc.Reports[0])
						paste := mc.Pasted
						a, err := c.Input(agent.Input{ID: "invisible", Text: mc.Text, Written: reconnect, WrittenText: &paste, Earlier: reconnect})
						if err != nil || !reconnect && (len(a.Keys) != 1 || a.Keys[0].Paste != paste || a.InputText["invisible"] != paste) || reconnect && len(a.Keys) != 0 {
							t.Fatalf("reconnect=%v paste: %+v, %v; captured %q", reconnect, a, err, paste)
						}
						a = c.Decode(agent.Hook, envelope("captured", "", mc.Reports[0]))
						if summary(a) != "turn(caller)" || a.Events[0].Input != "invisible" || c.sent != nil || c.holdsBox() || len(c.pasted) != 0 {
							t.Fatalf("reconnect=%v: %+v; sent %+v, holds=%v, pasted %+v", reconnect, a, c.sent, c.holdsBox(), c.pasted)
						}
						if a := c.Tick(time.Now().Add(time.Hour)); len(a.Events) != 0 || len(a.Keys) != 0 {
							t.Fatalf("delivered input expired or wrote keys: %+v", a)
						}
					}
					if strings.ContainsAny(mc.Prompts[0], "\u200d\ufe0f") {
						c := invisibleCodec(name, mc.Reports[0])
						c.Input(agent.Input{ID: "visible", Text: mc.Text})
						// Deliberately mutate a captured report: losing an emoji's
						// selector or a language's joiner must not claim its input.
						other := strings.NewReplacer("\u200d", "", "\ufe0f", "").Replace(mc.Prompts[0])
						if by, id := c.starter(other); by == agent.ByCaller {
							t.Fatalf("modified visible text claimed %q: %q", id, other)
						}
					}
				})
			}
		}
	}
}

// The captured warning has consumed no prompt. Expiry is not evidence of
// an empty editor: #837's fence must reject later text, also across another
// reconnect, rather than append it to this invisible-character input.
func TestInvisibleConfirmationDoesNotCombineTheNextInput(t *testing.T) {
	for _, name := range []string{"consecutive_ordinary_start", "consecutive_ordinary_resume", "consecutive_confined_start", "consecutive_confined_resume"} {
		var mc struct {
			Text, Pasted, After string
			FirstReports        []json.RawMessage `json:"first_reports"`
			Reports             []json.RawMessage `json:"reports"`
		}
		b, err := os.ReadFile(filepath.Join(invisibleDir, name, "outcome.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &mc); err != nil {
			t.Fatal(err)
		}
		if len(mc.FirstReports) != 0 || len(mc.Reports) != 1 {
			t.Fatalf("%s: baseline did not hold then combine the input", name)
		}
		var combined payload
		json.Unmarshal(mc.Reports[0], &combined)
		if combined.Prompt != shown(mc.Pasted)+mc.After {
			t.Fatalf("%s: baseline report %q, want concatenation of %q and %q", name, combined.Prompt, shown(mc.Pasted), mc.After)
		}
		for _, reconnect := range []bool{false, true} {
			c := invisibleCodec(name, mc.Reports[0])
			paste := mc.Pasted
			c.Input(agent.Input{ID: "held", Text: mc.Text, Written: reconnect, WrittenText: &paste})
			c.watch = true
			now := time.Now()
			c.Tick(now)
			if a := c.Tick(now.Add(pasteSettle)); !reconnect && keysOf(a) != "enter" || reconnect && len(a.Keys) != 0 {
				t.Fatalf("initial Enter reconnect=%v: %+v", reconnect, a)
			}
			screen := readScreen(t, filepath.Join(invisibleDir, name, "first.raw"))
			if !bytes.Contains(screen, []byte("review and press Enter to send")) {
				t.Fatal("capture has no invisible-character confirmation warning")
			}
			c.Decode(agent.Screen, screen)
			heldTurnEnds(t, c, now.Add(time.Second), 2*DefaultInputTimeout, name)
			for _, again := range []bool{false, true} {
				if again {
					c = invisibleCodec(name, mc.Reports[0])
					c.Input(agent.Input{ID: "held", Text: mc.Text, Written: true, WrittenText: &paste})
				}
				a, err := c.Input(agent.Input{ID: "next", Text: mc.After})
				if err != nil || len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Input != "next" || a.Events[0].Reason != agent.NotDelivered {
					t.Fatalf("next input reconnect=%v again=%v: %+v, %v", reconnect, again, a, err)
				}
			}
		}
	}
}

// These prompts came from the real CLI, before #916. Matching must use
// what that CLI reports, while the paste and persisted WrittenText retain
// every character (image recognition happens before invisible removal).
func TestInvisibleMentionPromptsAreAttributed(t *testing.T) {
	for _, name := range []string{"mention_ordinary", "mention_restricted_pasted", "mention_unrestricted_pasted"} {
		for _, mc := range mentionCases(t, name) {
			if mc.Case != "after_invisible" && mc.Case != "after_escape" {
				continue
			}
			t.Run(name+"/"+mc.Session+"/"+mc.Case, func(t *testing.T) {
				fresh := func() *codec {
					if name == "mention_ordinary" {
						return started(t)
					}
					return confinedCodec(t)
				}
				c := fresh()
				a, err := c.Input(agent.Input{ID: "invisible", Text: mc.Text})
				if err != nil || len(a.Keys) != 1 || a.Keys[0].Paste != mc.Pasted || a.InputText["invisible"] != mc.Pasted {
					t.Fatalf("paste: %+v, %v; captured %q", a, err, mc.Pasted)
				}
				for _, reconnect := range []bool{false, true} {
					if reconnect {
						c = fresh()
						paste := mc.Pasted
						if a, err := c.Input(agent.Input{ID: "invisible", Text: mc.Text, Written: true, WrittenText: &paste, Earlier: true}); err != nil || len(a.Keys) != 0 {
							t.Fatalf("reconnect: %+v, %v", a, err)
						}
					}
					if by, id := c.starter(mc.Prompt); by != agent.ByCaller || id != "invisible" || c.sent != nil || len(c.pasted) != 0 {
						t.Fatalf("reconnect=%v: prompt %q: %v/%q, sent %+v, pasted %+v", reconnect, mc.Prompt, by, id, c.sent, c.pasted)
					}
				}
			})
		}
	}
}

func TestEquivalentInvisiblePromptsKeepTheirOrder(t *testing.T) {
	for _, mc := range mentionCases(t, "mention_restricted_pasted") {
		if mc.Case != "after_invisible" {
			continue
		}
		for _, earlier := range []bool{false, true} {
			for _, saved := range []bool{false, true} {
				c := confinedCodec(t)
				for i, text := range []string{mc.Text, strings.ReplaceAll(mc.Text, "\u200b", ""), mc.Text} {
					paste := c.pasteText(text)
					var recorded *string
					if saved {
						recorded = &paste
					}
					_, err := c.Input(agent.Input{ID: []string{"first", "second", "third"}[i], Text: text, Written: true, WrittenText: recorded, Earlier: earlier})
					if err != nil {
						t.Fatal(err)
					}
				}
				for _, want := range []string{"first", "second", "third"} {
					if by, id := c.starter(mc.Prompt); by != agent.ByCaller || id != want {
						t.Fatalf("%s earlier=%v saved=%v: got %v/%q, want %q", mc.Session, earlier, saved, by, id, want)
					}
				}
			}
		}
	}
}
