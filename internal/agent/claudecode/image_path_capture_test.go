package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

type imagePathCase struct {
	Session, Case, Text, Pasted, Prompt string
	Images                              []string
}

func imagePathCases(t *testing.T, name string) []imagePathCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/2.1.332/image_paths", name, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []imagePathCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		key := c.Session + "/" + c.Case
		if c.Session != "start" && c.Session != "resume" || seen[key] {
			t.Fatalf("%s: invalid or repeated case %s", name, key)
		}
		seen[key] = true
	}
	for _, phase := range []string{"start", "resume"} {
		for _, name := range []string{"whole_inside", "line_inside", "whole_outside", "line_outside", "link_outside", "link_inside",
			"quoted", "single_quoted", "escaped", "escaped_extension", "invisible_before", "invisible_extension", "invisible_after",
			"terminal_sequence", "jpg", "jpeg", "gif", "webp", "two_paths", "relative", "wake"} {
			if !seen[phase+"/"+name] {
				t.Fatalf("capture has no %s/%s", phase, name)
			}
		}
	}
	if len(cases) != 42 {
		t.Fatalf("%s: got %d cases, want 42", name, len(cases))
	}
	return cases
}

// These real captures establish what the CLI attached before the adapter's
// rewrite. Restricted mode excludes outside paths and links to them, but
// still attaches images inside the directory, at a start and a resume.
func TestImagePathAttachmentCaptures(t *testing.T) {
	for _, name := range []string{"image_paths_ordinary", "image_paths_confined"} {
		for _, c := range imagePathCases(t, name) {
			want := 1
			switch c.Case {
			case "two_paths":
				want = 2
			case "relative", "wake", "invisible_before", "invisible_extension", "invisible_after":
				want = 0
			}
			if name == "image_paths_confined" {
				switch c.Case {
				case "whole_outside", "line_outside", "link_outside", "two_paths":
					want = 0
				}
			}
			if c.Pasted != c.Text || len(c.Images) != want {
				t.Errorf("%s/%s/%s: pasted %q for %q, images %q, want %d", name, c.Session, c.Case, c.Pasted, c.Text, c.Images, want)
			}
		}
	}
}

// The paste is the captured rewrite, including the text saved for a reconnect.
// No image reaches the API, also with the confined rewrite but no --restricted.
func TestImagePathPasteCaptures(t *testing.T) {
	for _, name := range []string{"image_paths_ordinary_pasted", "image_paths_confined_pasted", "image_paths_unrestricted_pasted"} {
		fresh := func() *codec {
			if name == "image_paths_ordinary_pasted" {
				return started(t)
			}
			return confinedCodec(t)
		}
		baselineName := "image_paths_confined"
		if name == "image_paths_ordinary_pasted" {
			baselineName = "image_paths_ordinary"
		}
		baseline := map[string]imagePathCase{}
		for _, c := range imagePathCases(t, baselineName) {
			baseline[c.Session+"/"+c.Case] = c
		}
		for _, captured := range imagePathCases(t, name) {
			t.Run(name+"/"+captured.Session+"/"+captured.Case, func(t *testing.T) {
				c := fresh()
				a, err := c.Input(agent.Input{ID: "image-text", Text: captured.Text})
				if err != nil || len(a.Keys) != 1 || a.Keys[0].Paste != captured.Pasted {
					t.Fatalf("paste: %v, %v; captured %q", a.Keys, err, captured.Pasted)
				}
				if text, ok := a.InputText["image-text"]; !ok || text != captured.Pasted {
					t.Fatalf("recorded paste %q (%v), want %q", text, ok, captured.Pasted)
				}
				if len(captured.Images) != 0 {
					t.Fatalf("images reached the API: %q", captured.Images)
				}
				// Attribution after the CLI removes invisible characters or an
				// ordinary paste's terminal sequence belongs to #916. Those
				// captures still check attachment prevention and the actual paste.
				if normalize(captured.Pasted) != normalize(unwrapPaste(captured.Prompt)) {
					switch captured.Case {
					case "invisible_before", "invisible_extension", "invisible_after", "terminal_sequence":
						return
					default:
						t.Fatalf("unexpected prompt change: %q -> %q", captured.Pasted, captured.Prompt)
					}
				}
				paste := captured.Pasted
				restarted := fresh()
				a, err = restarted.Input(agent.Input{ID: "image-text", Text: captured.Text, Written: true, WrittenText: &paste, Earlier: true})
				if err != nil || len(a.Keys) != 0 {
					t.Fatalf("reconnect: %v, %v", a, err)
				}
				for state, c := range map[string]*codec{"live": c, "reconnected": restarted} {
					old := baseline[captured.Session+"/"+captured.Case]
					if normalize(old.Prompt) != normalize(captured.Prompt) {
						if by, id := c.starter(old.Prompt); by == agent.ByCaller {
							t.Fatalf("%s: baseline prompt claimed rewritten input %q", state, id)
						}
					}
					if by, id := c.starter(captured.Prompt); by != agent.ByCaller || id != "image-text" || c.sent != nil || len(c.pasted) != 0 {
						t.Errorf("%s: prompt counts as %v, input %q; sent %v, pasted %v", state, by, id, c.sent, c.pasted)
					}
				}
			})
		}
	}
}
