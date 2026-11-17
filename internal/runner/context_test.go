package runner

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/agent"
)

// recorder is the fake adapter with every input its codecs are given
// recorded, by session state directory.
type recorder struct {
	agent.Adapter
	mu     sync.Mutex
	inputs map[string][]string
	ids    map[string][]string // the Input.ID of each, in the same order
}

type recordingCodec struct {
	agent.Codec
	r   *recorder
	dir string
}

func (r *recorder) NewCodec(s agent.Spec) agent.Codec {
	return &recordingCodec{Codec: r.Adapter.NewCodec(s), r: r, dir: s.StateDir}
}

func (c *recordingCodec) Input(in agent.Input) (agent.Actions, error) {
	c.r.mu.Lock()
	c.r.inputs[c.dir] = append(c.r.inputs[c.dir], in.Text)
	if c.r.ids == nil {
		c.r.ids = map[string][]string{}
	}
	c.r.ids[c.dir] = append(c.r.ids[c.dir], in.ID)
	c.r.mu.Unlock()
	return c.Codec.Input(in)
}

// idsOf returns the Input.IDs of the inputs of session id.
func (r *recorder) idsOf(root string, id int64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids[StateDir(root, id)]...)
}

func (r *recorder) of(root string, id int64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.inputs[StateDir(root, id)]...)
}

// A session that replaces one whose conversation is gone starts with the
// thread's inputs and final answers, oldest first and marked, then the
// message that woke it. Other sessions get no such context.
func TestReplacingSessionGetsEarlierContext(t *testing.T) {
	rec := &recorder{inputs: map[string][]string{}}
	conv := t.TempDir()
	f := newFixture(t, options{idlePeriod: 300 * time.Millisecond, conversations: conv,
		wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.start("reply hello")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	send(t, owner(f.core), thread, "m2", "reply more")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
	files, _ := filepath.Glob(filepath.Join(conv, "*"))
	for _, p := range files {
		removeConversation(t, p)
	}

	send(t, owner(f.core), thread, "m3", "reply anew")
	waitFor(t, "the new session's turn", 15*time.Second, f.has("turn_ended", 3))

	want := contextStart + "user: reply hello\nagent: hello\nuser: reply more\nagent: more\n" + contextEnd + "reply anew"
	if got := rec.of(f.root, 2); len(got) != 1 || got[0] != want {
		t.Fatalf("the replacing session's inputs: %q\nwant one: %q", got, want)
	}
	for _, in := range rec.of(f.root, 1) {
		if strings.Contains(in, contextStart) {
			t.Fatalf("the first session got earlier context: %q", in)
		}
	}
	if got := rec.of(f.root, 1); len(got) == 0 {
		t.Fatal("no input recorded for the first session")
	}
	// The input event records the message alone.
	ins := f.eventData("input")
	if last := ins[len(ins)-1]; last["text"] != "reply anew" {
		t.Fatalf("input events %v", ins)
	}
}

// The cap is in bytes, and the cut drops the oldest part, never half a
// character.
func TestEarlierContextCutDropsTheOldest(t *testing.T) {
	s := "user: first\nagent: ünïcode\nuser: last\n"
	got := markContext(s, 20)
	body := strings.TrimSuffix(strings.TrimPrefix(got, contextStart), contextEnd)
	if len(body) > 20 || !strings.HasSuffix(s, body) || !utf8.ValidString(body) || !strings.HasSuffix(body, "user: last\n") {
		t.Fatalf("cut to %q", body)
	}
	if strings.Contains(body, "first") {
		t.Fatalf("the oldest part is kept: %q", body)
	}
	if got := markContext(s, 1000); got != contextStart+s+contextEnd {
		t.Fatalf("uncut: %q", got)
	}
	if markContext("", 10) != "" {
		t.Fatal("context for an empty thread")
	}
}

// removeConversation removes a conversation file of the fake, as the
// agent's own retention would.
func removeConversation(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}
