package claudecode

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

// lines reads a capture file as lines.
func lines(t *testing.T, file string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/2.1.322", file))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

func hookFiles(t *testing.T, scenario string) []string {
	files, _ := filepath.Glob(filepath.Join("testdata/2.1.322", scenario, "hooks", "*.json"))
	for i, f := range files {
		files[i] = strings.TrimPrefix(f, "testdata/2.1.322/")
	}
	return files
}

// run feeds a scenario to c: its SessionStart, then either every
// transcript line and then the other hooks, or the other way round. It
// returns every action, and the transcripts the codec asked to follow.
func run(t *testing.T, c *codec, scenario string, transcriptFirst bool) (Actions, []string) {
	t.Helper()
	hooks := hookFiles(t, scenario)
	var all Actions
	all = merge(all, decodeFile(t, c, "h0", hooks[0]))
	rest := func() {
		for i, f := range hooks[1:] {
			all = merge(all, decodeFile(t, c, "h"+string(rune('a'+i)), f))
		}
	}
	transcript := func() {
		for _, l := range lines(t, filepath.Join(scenario, "transcript.jsonl")) {
			all = merge(all, c.Decode(agent.File, l))
		}
	}
	if transcriptFirst {
		transcript()
		rest()
	} else {
		rest()
		transcript()
	}
	return all, all.Follow
}

type textEv struct {
	text  string
	final bool
}

func texts(a Actions) []textEv {
	var out []textEv
	for _, e := range a.Events {
		if e.Kind == agent.Text {
			out = append(out, textEv{e.Text, e.Final})
		}
	}
	return out
}

func TestResumedTranscriptGivesNoOldText(t *testing.T) {
	for _, first := range []bool{false, true} {
		a, follow := run(t, newTestCodec(), "resume", first)
		got := texts(a)
		if len(follow) != 1 || !strings.HasSuffix(follow[0], ".jsonl") {
			t.Fatalf("followed %v", follow)
		}
		// The transcript holds the earlier turn ("ok") too; only the
		// resumed turn's answer comes out, once, as final.
		if len(got) != 1 || got[0] != (textEv{"again", true}) {
			t.Fatalf("transcript first=%v: texts %+v", first, got)
		}
	}
}

func TestFollowsANewPath(t *testing.T) {
	c := newTestCodec()
	start := decodeFile(t, c, "h1", "compact/hooks/01-SessionStart.json")
	again := decodeFile(t, c, "h2", "compact/hooks/06-SessionStart.json") // after compaction, same path
	if len(start.Follow) != 1 || len(again.Follow) != 0 {
		t.Fatalf("follow at start %v, after compaction %v", start.Follow, again.Follow)
	}
	other := []byte(`{"hook":"h3","wait":false,"payload":{"hook_event_name":"SessionStart","source":"clear","session_id":"s2","transcript_path":"/home/user/.claude/projects/p/s2.jsonl"}}`)
	if a := c.Decode(agent.Hook, other); len(a.Follow) != 1 || a.Follow[0] != "/home/user/.claude/projects/p/s2.jsonl" {
		t.Fatalf("a new path was not followed: %v", a.Follow)
	}
}

func TestTextInOrderAndFinalOnce(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		want     []textEv
	}{
		{"plain", []textEv{{"ok", true}}},
		{"tools", []textEv{{"done", true}}},
		// The subagent's own "sub-ok" is in its own transcript; the main
		// conversation repeats it in the turn the notification starts,
		// which ends with the session.
		{"subagent", []textEv{{"The subagent is still running. I'll reply with its result once the completion notification arrives.", true}, {"sub-ok", false}}},
	} {
		for _, first := range []bool{false, true} {
			a, _ := run(t, newTestCodec(), tc.scenario, first)
			got := texts(a)
			if len(got) != len(tc.want) {
				t.Fatalf("%s (transcript first=%v): %+v", tc.scenario, first, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("%s (transcript first=%v): text %d = %+v, want %+v", tc.scenario, first, i, got[i], tc.want[i])
				}
			}
		}
	}
}

func TestSubagentTranscriptAndUserLinesGiveNoText(t *testing.T) {
	c := newTestCodec()
	decodeFile(t, c, "h1", "subagent/hooks/01-SessionStart.json")
	decodeFile(t, c, "h2", "subagent/hooks/02-UserPromptSubmit.json")
	files, _ := filepath.Glob("testdata/2.1.322/subagent/subagent-*.jsonl")
	for _, l := range lines(t, strings.TrimPrefix(files[0], "testdata/2.1.322/")) {
		if a := c.Decode(agent.File, l); len(a.Events) != 0 {
			t.Fatalf("a subagent line produced %+v", a.Events)
		}
	}
	for _, l := range lines(t, "subagent/transcript.jsonl") {
		if bytes.Contains(l, []byte(`"type":"user"`)) {
			if a := c.Decode(agent.File, l); len(a.Events) != 0 {
				t.Fatalf("a user line produced %+v", a.Events)
			}
		}
	}
}

func TestInterruptEndsTheTurn(t *testing.T) {
	for _, first := range []bool{false, true} {
		a, _ := run(t, newTestCodec(), "interrupt", first)
		var ends []agent.Event
		for _, e := range a.Events {
			if e.Kind == agent.TurnEnded {
				ends = append(ends, e)
			}
		}
		if len(ends) != 1 || ends[0].Status != agent.Interrupted {
			t.Fatalf("transcript first=%v: turn ends %+v", first, ends)
		}
	}
}

func TestUnknownTranscriptLines(t *testing.T) {
	c := newTestCodec()
	for _, l := range []string{`{"type":"something-new","x":1}`, `not json`, `{"type":"assistant","message":{"content":"plain string"}}`, ``} {
		if a := c.Decode(agent.File, []byte(l)); len(a.Events) != 0 {
			t.Fatalf("%q produced %+v", l, a.Events)
		}
	}
}

// Without the transcript, the hooks alone still start and end every turn
// and carry the final answers.
func TestHooksAloneWithoutTranscript(t *testing.T) {
	for _, scenario := range []string{"plain", "tools", "background", "subagent", "resume"} {
		c := newTestCodec()
		var all Actions
		for i, f := range hookFiles(t, scenario) {
			all = merge(all, decodeFile(t, c, "h"+string(rune('a'+i)), f))
		}
		var started, ended, finals int
		for _, e := range all.Events {
			switch {
			case e.Kind == agent.TurnStarted:
				started++
			case e.Kind == agent.TurnEnded:
				ended++
			case e.Kind == agent.Text && e.Final && e.Text != "":
				finals++
			}
		}
		stops := 0
		for _, f := range hookFiles(t, scenario) {
			if strings.HasSuffix(f, "-Stop.json") {
				stops++
			}
		}
		if started == 0 || ended != stops || finals != stops {
			t.Errorf("%s: %d started, %d ended, %d final answers for %d Stop calls", scenario, started, ended, finals, stops)
		}
	}
}
