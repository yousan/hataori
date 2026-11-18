package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/host"
)

// reader is a runner and one session's state for read, without a host or a
// store: the files are written by the test.
type reader struct {
	t  *testing.T
	r  *Runner
	lv *live
}

func newReader(t *testing.T, codec agent.Codec) *reader {
	dir := t.TempDir()
	return &reader{t: t, r: &Runner{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		lv: &live{dir: dir, codec: codec, files: map[string]int64{}}}
}

func (x *reader) appendTo(path string, ls ...[]byte) {
	x.t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		x.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range ls {
		f.Write(append(append([]byte{}, l...), '\n'))
	}
}

func (x *reader) hooks(ls ...[]byte) { x.appendTo(filepath.Join(x.lv.dir, host.EventsFile), ls...) }

// step reads what is new and keeps the offsets, as a step that commits.
func (x *reader) step() []agent.Event {
	b := x.r.read(x.lv)
	x.lv.offset = b.offset
	for p, o := range b.files {
		x.lv.files[p] = o
	}
	return b.events
}

func hookLine(id string, payload string) []byte {
	return []byte(fmt.Sprintf(`{"hook":%q,"wait":false,"at":0,"payload":%s}`, id, payload))
}

func fakeTerminalCodec(t *testing.T) agent.Codec {
	t.Helper()
	cfg, _ := json.Marshal(fake.Config{Command: []string{"unused"}, Terminal: true})
	ad, err := fake.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ad.NewCodec(agent.Spec{})
}

func kinds(evs []agent.Event) []agent.Kind {
	var ks []agent.Kind
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return ks
}

// A transcript line written before a hook call, both new in one step, is
// decoded before the call.
func TestFollowedFileReadBeforeHookCalls(t *testing.T) {
	x := newReader(t, fakeTerminalCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
		hookLine("h2", `{"t":"turn","by":"caller"}`))
	x.step()

	x.appendTo(transcript, []byte(`{"t":"text","text":"hello","final":true}`))
	x.hooks(hookLine("h3", `{"t":"end","status":"completed"}`))
	got := kinds(x.step())
	if len(got) != 2 || got[0] != agent.Text || got[1] != agent.TurnEnded {
		t.Fatalf("events %v, want the text, then the end", got)
	}
}

// A file a hook call names is read in the same step, after the call.
func TestFileNamedByAHookReadInTheSameStep(t *testing.T) {
	x := newReader(t, fakeTerminalCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.appendTo(transcript, []byte(`{"t":"text","text":"hello","final":true}`))
	x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`))
	got := kinds(x.step())
	if len(got) != 2 || got[0] != agent.SessionReady || got[1] != agent.Text {
		t.Fatalf("events %v, want ready, then the named file's text", got)
	}
	if x.lv.files[transcript] == 0 {
		t.Fatal("the named file's offset was not kept")
	}
}

// The Claude Code capture of a turn with two model messages: the line of
// the final message and the Stop hook come in one step. The final
// message's tokens are reported before the turn ends.
func TestFinalMessageUsageBeforeTheTurnEnds(t *testing.T) {
	const capture = "../agent/claudecode/testdata/2.1.327/usage"
	hookFiles, _ := filepath.Glob(filepath.Join(capture, "hooks", "*.json"))
	if len(hookFiles) != 10 {
		t.Fatalf("%d hook files", len(hookFiles))
	}
	call := func(n int) []byte {
		b, err := os.ReadFile(hookFiles[n-1])
		if err != nil {
			t.Fatal(err)
		}
		return hookLine(fmt.Sprint("c", n), string(bytes.TrimSpace(b)))
	}
	raw, err := os.ReadFile(filepath.Join(capture, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	// The first turn ends before the user line of the second prompt; its
	// final message is the last assistant line before that.
	var second struct {
		PromptID string `json:"prompt_id"`
	}
	b, _ := os.ReadFile(hookFiles[7]) // 08-UserPromptSubmit
	json.Unmarshal(b, &second)
	final, end := -1, -1
	for i, l := range lines {
		var tl struct {
			Type     string `json:"type"`
			PromptID string `json:"promptId"`
		}
		json.Unmarshal(l, &tl)
		if tl.Type == "user" && tl.PromptID == second.PromptID {
			end = i
			break
		}
		if tl.Type == "assistant" {
			final = i
		}
	}
	if final < 0 || end < 0 {
		t.Fatal("the capture's first turn was not found")
	}

	ad, err := claudecode.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	x := newReader(t, ad.NewCodec(agent.Spec{}))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.lv.files[transcript] = 0
	x.hooks(call(1), call(2))
	x.appendTo(transcript, lines[:final]...)
	evs := x.step()

	x.appendTo(transcript, lines[final:end]...)
	x.hooks(call(3), call(4), call(5), call(6), call(7))
	evs = append(evs, x.step()...)

	var last *agent.UsageCount
	ended := false
	for _, e := range evs {
		switch e.Kind {
		case agent.Usage:
			last = e.Usage
		case agent.TurnEnded:
			ended = true
			// By hand from the capture: two messages, 2+2 input, 145+3
			// output, 27435+36046 cache reads, 8611+227 cache writes.
			want := agent.UsageCount{Model: "claude-haiku-5-5", Input: 4, Output: 148, CacheRead: 63481, CacheWrite: 8838}
			if last == nil || *last != want {
				t.Fatalf("usage before the end %+v, want %+v", last, want)
			}
		}
	}
	if !ended {
		t.Fatalf("the turn did not end: %v", kinds(evs))
	}
}
