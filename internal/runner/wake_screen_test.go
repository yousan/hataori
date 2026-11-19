package runner

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// screens is the fake adapter with what each of its codecs was given of
// the screen recorded, in the order the codecs were made.
type screens struct {
	agent.Adapter
	mu  sync.Mutex
	got []*bytes.Buffer
}

type screenCodec struct {
	agent.Codec
	s   *screens
	buf *bytes.Buffer
}

func (s *screens) NewCodec(spec agent.Spec) agent.Codec {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &bytes.Buffer{}
	s.got = append(s.got, b)
	return &screenCodec{Codec: s.Adapter.NewCodec(spec), s: s, buf: b}
}

func (c *screenCodec) Decode(src agent.Source, line []byte) agent.Actions {
	if src == agent.Screen {
		c.s.mu.Lock()
		c.buf.Write(line)
		c.s.mu.Unlock()
	}
	return c.Codec.Decode(src, line)
}

func (s *screens) of(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.got) {
		return nil
	}
	return bytes.Clone(s.got[i].Bytes())
}

// The codec of an agent started again after a sleep is given only what
// that agent writes to its terminal. The session host appends every run's
// output to the same log, and a codec that read the last run's screen in
// front of the new one would look for the start-up dialog in both (#505).
func TestWokenCodecReadsOnlyItsOwnScreen(t *testing.T) {
	const dialog = "fake: trust this folder? Enter"
	rec := &screens{}
	f := newFixture(t, options{terminal: true, dialog: dialog, idlePeriod: 300 * time.Millisecond,
		wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.start("screen the first run's screen\nreply hello")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
	if first := rec.of(0); !bytes.Contains(first, []byte(dialog)) {
		t.Fatalf("the first codec was not given the dialog: %q", first)
	}

	send(t, owner(f.core), thread, "m2", "reply again")
	waitFor(t, "the resumed turn", 15*time.Second, f.has("turn_ended", 2))
	var woken []byte
	rec.mu.Lock()
	n := len(rec.got)
	rec.mu.Unlock()
	for i := 1; i < n; i++ {
		if b := rec.of(i); len(b) > 0 {
			if woken != nil {
				t.Fatalf("more than one later codec was given the screen")
			}
			woken = b
		}
	}
	if !bytes.HasPrefix(woken, []byte(dialog)) {
		t.Fatalf("the woken codec's screen does not start with its agent's dialog: %q", woken)
	}
	if bytes.Contains(woken, []byte("the first run's screen")) || bytes.Count(woken, []byte("fake: answered")) != 1 {
		t.Fatalf("the woken codec was given the first run's screen: %q", woken)
	}
}
