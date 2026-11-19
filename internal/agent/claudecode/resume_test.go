package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

func resumeCodec(id string) *codec {
	return (&Adapter{}).NewCodec(agent.Spec{Approvals: agent.AllowAll, ResumeID: id}).(*codec)
}

func sessionOf(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/2.1.322", file))
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	json.Unmarshal(b, &p)
	return p.SessionID
}

// The SessionStart of a resume must name the session asked for.
func TestResumeReportsTheSameID(t *testing.T) {
	const file = "sleep_resumed/hooks/01-SessionStart.json"
	id := sessionOf(t, file)
	if got := summary(decodeFile(t, resumeCodec(id), "h1", file)); got != "ready(true,,)" {
		t.Fatalf("resume of %s: %s", id, got)
	}
	if got := summary(decodeFile(t, resumeCodec("00000000-0000-4000-8000-999999999999"), "h1", file)); got != "session-end(resumed another session)" {
		t.Fatalf("resume reporting another id: %s", got)
	}
}

// Asked to resume a conversation it no longer has, the CLI reports only a
// SessionEnd: that is "cannot resume". A SessionEnd after the start is an
// ordinary end with its reason.
func TestCannotResume(t *testing.T) {
	const missing = "resume_missing/hooks/01-SessionEnd.json"
	if got := summary(decodeFile(t, resumeCodec(sessionOf(t, missing)), "h1", missing)); got != "session-end(cannot resume)" {
		t.Fatalf("resume of a missing conversation: %s", got)
	}
	c := resumeCodec(sessionOf(t, "sleep_resumed/hooks/01-SessionStart.json"))
	decodeFile(t, c, "h1", "sleep_resumed/hooks/01-SessionStart.json")
	if got := summary(decodeFile(t, c, "h2", "sleep/hooks/04-SessionEnd.json")); got != "session-end(other)" {
		t.Fatalf("an end after the start: %s", got)
	}
}

// feed decodes a scenario's hooks in order, with each transcript read
// right after the hook whose number is given.
func feed(t *testing.T, c *codec, scenario string, after map[int]string) Actions {
	t.Helper()
	var all Actions
	for i, f := range hookFiles(t, scenario) {
		all = merge(all, decodeFile(t, c, "h"+string(rune('a'+i)), f))
		if tr, ok := after[i+1]; ok {
			for _, l := range lines(t, filepath.Join(scenario, tr)) {
				all = merge(all, c.Decode(agent.File, l))
			}
		}
	}
	return all
}

// After a person clears the conversation at the terminal, the codec
// follows the new session: its transcript, its turns and its id.
func TestClearIsFollowed(t *testing.T) {
	// Each transcript read after its turn's Stop, or before it.
	for _, after := range []map[int]string{
		{3: "transcript.jsonl", 7: "transcript-2.jsonl"},
		{2: "transcript.jsonl", 6: "transcript-2.jsonl"},
	} {
		clearFollowed(t, after)
	}
}

func clearFollowed(t *testing.T, after map[int]string) {
	t.Helper()
	c := newTestCodec()
	all := feed(t, c, "clear", after)
	if len(all.Follow) != 2 || all.Follow[0] == all.Follow[1] {
		t.Fatalf("followed %v", all.Follow)
	}
	var ready []string
	for _, e := range all.Events {
		if e.Kind == agent.SessionReady {
			ready = append(ready, e.SessionID)
		}
	}
	if len(ready) != 2 || ready[0] == ready[1] || ready[1] != sessionOf(t, "clear/hooks/05-SessionStart.json") {
		t.Fatalf("ready ids %v", ready)
	}
	if got := texts(all); len(got) != 2 || got[0] != (textEv{"ok", true}) || got[1] != (textEv{"again", true}) {
		t.Fatalf("transcripts after hooks %v: texts %+v", after, got)
	}
}

// After a clear or a compaction, input goes to the CLI at once: the codec
// still counts the session as started.
func TestInputAfterClearOrCompaction(t *testing.T) {
	for scenario, upTo := range map[string]int{"clear": 5, "compact": 7} {
		c := newTestCodec()
		for i, f := range hookFiles(t, scenario)[:upTo] {
			decodeFile(t, c, "h"+string(rune('a'+i)), f)
		}
		a, err := c.Input(agent.Input{Text: "next"})
		if err != nil || len(a.Keys) == 0 {
			t.Errorf("%s: input held or refused: %+v, %v", scenario, a, err)
		}
	}
}
