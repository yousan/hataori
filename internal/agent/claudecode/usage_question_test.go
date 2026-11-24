package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// coreTurns counts the tokens of the core's turns from a codec's events,
// the way the core does (internal/session): a question ends the open turn,
// its answer or its withdrawal opens the next, each usage event is the
// total of the open turn, and a Late one that of the turn that ended last.
type coreTurns struct {
	tokens  []int64
	open    bool
	waiting bool
	ended   int // the turn that ended last, from 1
}

func (k *coreTurns) start() {
	if !k.open {
		k.tokens, k.open = append(k.tokens, 0), true
	}
	k.waiting = false
}

func (k *coreTurns) feed(t *testing.T, a Actions) {
	t.Helper()
	for _, e := range a.Events {
		switch e.Kind {
		case agent.TurnStarted:
			k.start()
		case agent.Request:
			k.open, k.waiting, k.ended = false, true, len(k.tokens)
		case agent.RequestWithdrawn:
			if k.waiting {
				k.start()
			}
		case agent.TurnEnded:
			k.open, k.waiting, k.ended = false, false, len(k.tokens)
		case agent.Usage:
			n := len(k.tokens)
			if e.Late {
				n = k.ended
			} else if !k.open {
				t.Errorf("usage %+v outside a turn, not Late", *e.Usage)
				continue
			}
			u := e.Usage
			k.tokens[n-1] = max(k.tokens[n-1], u.Input+u.Output+u.CacheRead+u.CacheWrite)
		}
	}
}

// questionCapture is a capture of one prompt with one question: the
// transcript up to the line of the message that asks, that line, and what
// follows up to and with the line of the answer's message.
type questionCapture struct {
	dir               string
	hooks             []string
	start, asks, rest [][]byte
}

func readQuestion(t *testing.T, name string) questionCapture {
	t.Helper()
	q := questionCapture{dir: filepath.Join("testdata", "2.1.322", name)}
	q.hooks, _ = filepath.Glob(filepath.Join(q.dir, "hooks", "*.json"))
	b, err := os.ReadFile(filepath.Join(q.dir, "transcript.jsonl"))
	if err != nil || len(q.hooks) == 0 {
		t.Fatal(err, len(q.hooks))
	}
	var at []int
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i, l := range lines {
		var tl transcriptLine
		json.Unmarshal(l, &tl)
		if tl.Type == "assistant" {
			at = append(at, i)
		}
	}
	if len(at) != 2 {
		t.Fatalf("%s: %d assistant lines, want 2", name, len(at))
	}
	q.start, q.asks, q.rest = lines[:at[0]], lines[at[0]:at[0]+1], lines[at[0]+1:at[1]+1]
	return q
}

// hook gives the codec the capture's n-th hook call, with edits to its
// payload, and the id of the call.
func (q questionCapture) hook(t *testing.T, c *codec, n int, id string, edit map[string]string) Actions {
	t.Helper()
	b, err := os.ReadFile(q.hooks[n-1])
	if err != nil {
		t.Fatal(err)
	}
	for old, new := range edit {
		if !bytes.Contains(b, []byte(old)) {
			t.Fatalf("%s has no %q", q.hooks[n-1], old)
		}
		b = bytes.ReplaceAll(b, []byte(old), []byte(new))
	}
	var p payload
	json.Unmarshal(b, &p)
	return c.Decode(agent.Hook, questionEnvelope(t, id, p.Event == "PreToolUse", b))
}

func questionEnvelope(t *testing.T, id string, wait bool, payload []byte) []byte {
	t.Helper()
	var one bytes.Buffer
	if err := json.Compact(&one, payload); err != nil {
		t.Fatal(err)
	}
	return []byte(fmt.Sprintf(`{"hook":%q,"wait":%v,"at":0,"payload":%s}`, id, wait, one.Bytes()))
}

// The tokens of the messages of the captures, by hand from their
// transcripts: the one that asks, and the one after the answer.
var questionTokens = map[string][]int64{
	"question":           {2 + 125 + 29163 + 5144, 2 + 3 + 34307 + 186},
	"question-free":      {2 + 120 + 29163 + 5156, 2 + 11 + 34319 + 206},
	"question-multi":     {2 + 137 + 29163 + 5163, 2 + 10 + 34326 + 203},
	"question-timeout-q": {2 + 125 + 29163 + 5148, 2 + 3 + 34311 + 186},
}

// A question ends the core's turn, and its answer opens the next. Each
// message's tokens count once, in the turn it belongs to, wherever the
// line of the message that asks is read before the CLI goes on, and
// wherever the line after the answer is read (#685).
func TestUsageOfAQuestionCountsOnce(t *testing.T) {
	for _, name := range []string{"question", "question-free", "question-multi"} {
		for asks := 2; asks <= 5; asks++ { // after hook 2, hook 3 (asked), the answer, hook 4 (went on)
			for _, rest := range []int{4, 5} { // after hook 4, or hook 5 (Stop)
				q := readQuestion(t, name)
				c := newTestCodec()
				var k coreTurns
				at := func(step int, a Actions) {
					t.Helper()
					k.feed(t, a)
					if step == asks {
						k.feed(t, feedLines(c, q.asks))
					}
				}
				at(1, q.hook(t, c, 1, "h1", nil))
				k.feed(t, feedLines(c, q.start))
				at(2, q.hook(t, c, 2, "h2", nil))
				at(3, q.hook(t, c, 3, "h3", nil))
				a, err := c.Answer(agent.Answer{Request: "h3", Text: "Red"})
				if err != nil {
					t.Fatal(err)
				}
				k.start()
				at(4, a)
				at(5, q.hook(t, c, 4, "h4", nil))
				if rest == 4 {
					k.feed(t, feedLines(c, q.rest))
				}
				k.feed(t, q.hook(t, c, 5, "h5", nil))
				if rest == 5 {
					k.feed(t, feedLines(c, q.rest))
				}
				want := questionTokens[name]
				if asks == 5 { // read once the next turn runs: that turn's, and still once
					want = []int64{0, want[0] + want[1]}
				}
				if fmt.Sprint(k.tokens) != fmt.Sprint(want) {
					t.Errorf("%s, the question's line read at %d, the answer's at %d: turns of %v tokens, want %v", name, asks, rest, k.tokens, want)
				}
			}
		}
	}
}

// The same for a question that was not answered from here: withdrawn after
// its hook's timeout and answered in the terminal.
func TestUsageOfAWithdrawnQuestionCountsOnce(t *testing.T) {
	const name = "question-timeout-q"
	for _, tick := range []bool{true, false} { // withdrawn by the deadline, or by the tool's end
		q := readQuestion(t, name)
		c := newTestCodec()
		c.hookTimeout = time.Minute
		var k coreTurns
		k.feed(t, q.hook(t, c, 1, "h1", nil))
		k.feed(t, feedLines(c, q.start))
		k.feed(t, q.hook(t, c, 2, "h2", nil))
		k.feed(t, feedLines(c, q.asks))
		k.feed(t, q.hook(t, c, 3, "h3", nil))
		if tick {
			now := time.Now()
			k.feed(t, c.Tick(now))
			k.feed(t, c.Tick(now.Add(time.Hour)))
			if len(k.tokens) != 2 {
				t.Fatal("not withdrawn by the deadline")
			}
		}
		for n := 4; n <= 6; n++ {
			k.feed(t, q.hook(t, c, n, fmt.Sprint("h", n), nil))
		}
		k.feed(t, feedLines(c, q.rest))
		k.feed(t, q.hook(t, c, 7, "h7", nil))
		if want := questionTokens[name]; fmt.Sprint(k.tokens) != fmt.Sprint(want) {
			t.Errorf("tick %v: turns of %v tokens, want %v", tick, k.tokens, want)
		}
	}
}

// Two questions in one prompt make three turns. The second is the capture's
// own, asked again under other ids, also without a hook that said the first
// one's tool ended.
func TestUsageOfTwoQuestionsCountsOnce(t *testing.T) {
	q := readQuestion(t, "question")
	var p payload
	b, _ := os.ReadFile(q.hooks[2])
	json.Unmarshal(b, &p)
	var l transcriptLine
	json.Unmarshal(q.asks[0], &l)
	if p.ToolUseID == "" || l.Message.ID == "" || l.UUID == "" {
		t.Fatalf("tool use %q, message %q, line %q", p.ToolUseID, l.Message.ID, l.UUID)
	}
	other := map[string]string{p.ToolUseID: p.ToolUseID + "x"}
	line := bytes.ReplaceAll(q.asks[0], []byte(l.Message.ID), []byte(l.Message.ID+"x"))
	again := [][]byte{bytes.ReplaceAll(line, []byte(l.UUID), []byte(l.UUID+"x"))}
	for _, ended := range []bool{true, false} {
		c := newTestCodec()
		var k coreTurns
		answer := func(id string) {
			if _, err := c.Answer(agent.Answer{Request: id, Text: "Red"}); err != nil {
				t.Fatal(err)
			}
			k.start()
		}
		k.feed(t, q.hook(t, c, 1, "h1", nil))
		k.feed(t, feedLines(c, q.start))
		k.feed(t, q.hook(t, c, 2, "h2", nil))
		k.feed(t, feedLines(c, q.asks))
		k.feed(t, q.hook(t, c, 3, "h3", nil))
		answer("h3")
		if ended {
			k.feed(t, q.hook(t, c, 4, "h4", nil))
			k.feed(t, feedLines(c, again))
		}
		k.feed(t, q.hook(t, c, 3, "h3x", other))
		if !ended {
			k.feed(t, feedLines(c, again)) // read while the second waits
		}
		answer("h3x")
		k.feed(t, q.hook(t, c, 4, "h4x", other))
		k.feed(t, feedLines(c, q.rest))
		k.feed(t, q.hook(t, c, 5, "h5", nil))
		one := questionTokens["question"]
		if want := []int64{one[0], one[0], one[1]}; fmt.Sprint(k.tokens) != fmt.Sprint(want) {
			t.Errorf("the first question's end reported %v: turns of %v tokens, want %v", ended, k.tokens, want)
		}
	}
}

// A question ended by an interrupt, or with the session: nothing after it
// counts what the asking turn counted. The interrupt line is the interrupt
// capture's, with the prompt's id.
func TestUsageOfAQuestionCutShortCountsOnce(t *testing.T) {
	q := readQuestion(t, "question")
	var p payload
	b, _ := os.ReadFile(q.hooks[2])
	json.Unmarshal(b, &p)
	var stop []byte
	b, _ = os.ReadFile(filepath.Join("testdata", "2.1.322", "interrupt", "transcript.jsonl"))
	for _, l := range bytes.Split(b, []byte("\n")) {
		var tl transcriptLine
		json.Unmarshal(l, &tl)
		if tl.Type == "user" && bytes.Contains(l, []byte("[Request interrupted by user for tool use]")) {
			var m map[string]any
			json.Unmarshal(l, &m)
			m["promptId"], m["sessionId"] = p.PromptID, p.SessionID
			stop, _ = json.Marshal(m)
		}
	}
	if stop == nil {
		t.Fatal("no interrupt line")
	}
	one := questionTokens["question"]
	for _, how := range []string{"interrupt", "session end"} {
		for _, answered := range []bool{false, true} {
			c := newTestCodec()
			var k coreTurns
			k.feed(t, q.hook(t, c, 1, "h1", nil))
			k.feed(t, feedLines(c, q.start))
			k.feed(t, q.hook(t, c, 2, "h2", nil))
			k.feed(t, feedLines(c, q.asks))
			k.feed(t, q.hook(t, c, 3, "h3", nil))
			if answered {
				if _, err := c.Answer(agent.Answer{Request: "h3", Text: "Red"}); err != nil {
					t.Fatal(err)
				}
				k.start()
			}
			want := []int64{one[0], 0}
			if how == "interrupt" {
				k.feed(t, c.Decode(agent.File, stop))
				// Read after it: the ended turn's, as one after a Stop.
				k.feed(t, feedLines(c, q.rest))
				want[1] = one[1]
			} else {
				k.feed(t, q.hook(t, c, 6, "h6", nil))
			}
			if fmt.Sprint(k.tokens) != fmt.Sprint(want) {
				t.Errorf("%s, answered %v: turns of %v tokens, want %v", how, answered, k.tokens, want)
			}
		}
	}
}

// After a restart the codec reads the lines again, without the answer
// given: the usage it then reports is the same.
func TestUsageOfAQuestionAfterARestart(t *testing.T) {
	q := readQuestion(t, "question")
	var got []string
	for _, answered := range []bool{true, false} {
		c := newTestCodec()
		q.hook(t, c, 1, "h1", nil)
		feedLines(c, q.start)
		q.hook(t, c, 2, "h2", nil)
		feedLines(c, q.asks)
		q.hook(t, c, 3, "h3", nil)
		if answered {
			if _, err := c.Answer(agent.Answer{Request: "h3", Text: "Red"}); err != nil {
				t.Fatal(err)
			}
		}
		q.hook(t, c, 4, "h4", nil)
		var s []string
		for _, e := range feedLines(c, q.rest).Events {
			if e.Kind == agent.Usage {
				s = append(s, fmt.Sprintf("%+v late %v", *e.Usage, e.Late))
			}
		}
		got = append(got, strings.Join(s, "; "))
	}
	const want = "{Model:claude-haiku-5-5 Input:2 Output:3 CacheRead:34307 CacheWrite:186 ContextUsed:0 ContextSize:0} late false"
	if got[0] != want || got[1] != want {
		t.Errorf("usage after the answer:\nrunning on    %s\nread again    %s\nwant          %s", got[0], got[1], want)
	}
}
