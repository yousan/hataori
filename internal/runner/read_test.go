package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	// The fake creates its transcript before reporting it. Preserve that
	// ordering: a missing reported source now pauses the entire batch.
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0600); err != nil {
		t.Fatal(err)
	}
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
	for _, rd := range b.reads {
		if rd.path == "" && rd.end != b.offset {
			x.t.Errorf("the log's range ends at %d and the offset is %d", rd.end, b.offset)
		}
	}
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

// whileReading is a codec that calls do before it decodes a line of a
// followed file, with how many such lines it was given before: while the
// step reads the files.
type whileReading struct {
	agent.Codec
	lines int
	do    func(n int)
}

func (c *whileReading) Decode(src agent.Source, l []byte) agent.Actions {
	if src == agent.File {
		c.lines++
		c.do(c.lines - 1)
	}
	return c.Codec.Decode(src, l)
}

// A hook call and the transcript line ahead of it, both written while a
// step reads the followed files, are not decoded with the call first, and
// none is lost (#917). Each line decoded has the next line and its call
// written, so however often a step looks at the files, one line is too late
// for it.
func TestHookCallsWrittenWhileFilesAreRead(t *testing.T) {
	const n = 20
	x := newReader(t, fakeTerminalCodec(t))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
		hookLine("h2", `{"t":"turn","by":"caller"}`))
	x.step()

	write := func(i int) {
		if i < n {
			x.appendTo(transcript, textLine(fmt.Sprint("line ", i)))
			x.hooks(hookLine(fmt.Sprint("c", i), string(textLine(fmt.Sprint("call ", i)))))
		}
	}
	x.lv.codec = &whileReading{Codec: x.lv.codec, do: func(i int) { write(i + 1) }}
	write(0)
	var got []string
	for range n + 1 {
		got = append(got, texts(x.step())...)
	}
	for i := range n {
		line, call := slices.Index(got, fmt.Sprint("line ", i)), slices.Index(got, fmt.Sprint("call ", i))
		if line < 0 || call < line {
			t.Errorf("line %d is text %d, its call %d: want both, and the line first", i, line, call)
		}
	}
	if len(got) != 2*n {
		t.Errorf("%d texts, want %d: %q", len(got), 2*n, got)
	}
}

// The same with a Claude Code capture of a question: the line of the message
// that asks and the PostToolUse call, which comes once the question is
// answered. The message's tokens are the asking turn's, which has ended.
func TestQuestionsUsageWhileFilesAreRead(t *testing.T) {
	const capture = "../agent/claudecode/testdata/2.1.322/question"
	hookFiles, _ := filepath.Glob(filepath.Join(capture, "hooks", "*.json"))
	raw, err := os.ReadFile(filepath.Join(capture, "transcript.jsonl"))
	if err != nil || len(hookFiles) != 6 {
		t.Fatal(err, len(hookFiles))
	}
	call := func(n int) []byte { // 1 SessionStart, 2 UserPromptSubmit, 3 PreToolUse, 4 PostToolUse
		b, err := os.ReadFile(hookFiles[n-1])
		if err != nil {
			t.Fatal(err)
		}
		var one bytes.Buffer
		if err := json.Compact(&one, b); err != nil {
			t.Fatal(err)
		}
		return []byte(fmt.Sprintf(`{"hook":"c%d","wait":%v,"at":0,"payload":%s}`, n, n == 3, one.Bytes()))
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	asks := slices.IndexFunc(lines, func(l []byte) bool {
		var tl struct{ Type string }
		json.Unmarshal(l, &tl)
		return tl.Type == "assistant"
	})
	if asks < 3 {
		t.Fatalf("the line that asks is line %d", asks)
	}

	ad, err := claudecode.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	x := newReader(t, ad.NewCodec(agent.Spec{}))
	transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
	x.hooks(call(1), call(2), call(3))
	x.appendTo(transcript, lines[:asks-2]...)
	x.lv.files[transcript] = 0
	x.step()
	// Not the file the capture's calls name: the step would look once more.
	x.lv.files = map[string]int64{transcript: x.lv.files[transcript]}
	if _, err := x.lv.codec.Answer(agent.Answer{Request: "c3", Text: "Red"}); err != nil {
		t.Fatal(err)
	}

	// A step looks at one followed file twice: the second look finds the
	// line before the one that asks, and that one is too late for it.
	x.lv.codec = &whileReading{Codec: x.lv.codec, do: func(n int) {
		switch n {
		case 0:
			x.appendTo(transcript, lines[asks-1])
		case 1:
			x.appendTo(transcript, lines[asks])
			x.hooks(call(4))
		}
	}}
	x.appendTo(transcript, lines[asks-2])
	var usage []agent.Event
	for _, e := range append(x.step(), x.step()...) {
		if e.Kind == agent.Usage {
			usage = append(usage, e)
		}
	}
	// By hand from the capture.
	want := agent.UsageCount{Model: "claude-haiku-5-5", Input: 2, Output: 125, CacheRead: 29163, CacheWrite: 5144}
	if len(usage) != 1 || *usage[0].Usage != want || !usage[0].Late {
		t.Fatalf("usage %+v, want one of %+v, Late", usage, want)
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

// partial writes b to the file without a newline.
func (x *reader) partial(path string, b []byte) {
	x.t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		x.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		x.t.Fatal(err)
	}
}

// texts are the texts of the Text events among evs.
func texts(evs []agent.Event) []string {
	var ts []string
	for _, e := range evs {
		if e.Kind == agent.Text {
			ts = append(ts, e.Text)
		}
	}
	return ts
}

func textLine(s string) []byte {
	return []byte(fmt.Sprintf(`{"t":"text","text":%q,"final":false}`, s))
}

// bothSources runs f for a followed file and for the host's log: write
// appends a text as a line of it, partial the beginning of one, path is the
// file, and offset what is stored for it.
func bothSources(t *testing.T, f func(t *testing.T, x *reader, path string, line func(string) []byte, offset func() int64)) {
	start := func(t *testing.T) (*reader, string) {
		x := newReader(t, fakeTerminalCodec(t))
		transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
		x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
			hookLine("h2", `{"t":"turn","by":"caller"}`))
		x.step()
		return x, transcript
	}
	t.Run("a followed file", func(t *testing.T) {
		x, p := start(t)
		f(t, x, p, textLine, func() int64 { return x.lv.files[p] })
	})
	t.Run("the host's log", func(t *testing.T) {
		x, _ := start(t)
		line := func(s string) []byte { return hookLine("h-"+s, string(textLine(s))) }
		f(t, x, filepath.Join(x.lv.dir, host.EventsFile), line, func() int64 { return x.lv.offset })
	})
}

// A last line without its newline is left for a later step, however often
// it is looked at and however it grows.
func TestLineWithoutItsEndIsLeft(t *testing.T) {
	bothSources(t, func(t *testing.T, x *reader, path string, line func(string) []byte, offset func() int64) {
		x.appendTo(path, line("one"))
		x.partial(path, line("two")[:10])
		if got := texts(x.step()); len(got) != 1 || got[0] != "one" {
			t.Fatalf("texts %q, want one", got)
		}
		at := offset()
		x.step()
		x.partial(path, line("two")[10:])
		for range 2 {
			if got := texts(x.step()); len(got) != 0 || offset() != at {
				t.Fatalf("texts %q at offset %d, want none and still %d: the line has no end yet", got, offset(), at)
			}
		}
		x.appendTo(path, nil) // the newline
		x.partial(path, line("three"))
		if got := texts(x.step()); len(got) != 1 || got[0] != "two" {
			t.Fatalf("texts %q, want two", got)
		}
		x.appendTo(path, nil)
		if got := texts(x.step()); len(got) != 1 || got[0] != "three" {
			t.Fatalf("texts %q, want three", got)
		}
		// What is written of a line may be replaced by a whole one that is
		// as long: the file's size does not say that nothing is new.
		four := line("four")
		x.partial(path, append(append([]byte{}, four...), ' '))
		x.step()
		if err := os.Truncate(path, offset()); err != nil {
			t.Fatal(err)
		}
		x.appendTo(path, four)
		if got := texts(x.step()); len(got) != 1 || got[0] != "four" {
			t.Fatalf("texts %q, want four", got)
		}
		if st, _ := os.Stat(path); offset() != st.Size() {
			t.Fatalf("offset %d of a file of %d", offset(), st.Size())
		}
	})
}

// A file that became shorter than the stored offset is not read until it
// is longer again, and then from that offset.
func TestFileShorterThanTheOffset(t *testing.T) {
	bothSources(t, func(t *testing.T, x *reader, path string, line func(string) []byte, offset func() int64) {
		x.appendTo(path, line("one"), line("two"))
		x.step()
		at := offset()
		if err := os.Truncate(path, at-5); err != nil {
			t.Fatal(err)
		}
		if got := x.step(); len(got) != 0 || offset() != at {
			t.Fatalf("events %v at offset %d, want none and still %d", kinds(got), offset(), at)
		}
		x.partial(path, []byte("xxxx\n")) // as long as the offset again
		if got := x.step(); len(got) != 0 || offset() != at {
			t.Fatalf("events %v at offset %d, want none and still %d", kinds(got), offset(), at)
		}
		x.appendTo(path, line("three"))
		if got := texts(x.step()); len(got) != 1 || got[0] != "three" {
			t.Fatalf("texts %q, want three", got)
		}
	})
}
