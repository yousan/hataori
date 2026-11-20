package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
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

// The fake's start-up dialog in the tests of a wake.
const wakeDialog = "fake: trust this folder? Enter"

// The codec of an agent started again after a sleep is given only what
// that agent writes to its terminal. The session host appends every run's
// output to the same log, and a codec that read the last run's screen in
// front of the new one would look for the start-up dialog in both (#505).
func TestWokenCodecReadsOnlyItsOwnScreen(t *testing.T) {
	rec := &screens{}
	f := newFixture(t, options{terminal: true, dialog: wakeDialog, idlePeriod: 300 * time.Millisecond,
		wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.start("screen the first run's screen\nreply hello")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
	if first := rec.of(0); !bytes.Contains(first, []byte(wakeDialog)) {
		t.Fatalf("the first codec was not given the dialog: %q", first)
	}
	send(t, owner(f.core), thread, "m2", "reply again")
	waitFor(t, "the resumed turn", 15*time.Second, f.has("turn_ended", 2))
	onlyItsOwnScreen(t, rec, 1)
}

// The same when the daemon died during the launch that wakes the session,
// before or after it started the new host: the daemon that comes back
// begins the launch again, and its codec too reads only the new agent's
// screen.
func TestWokenAfterADaemonKilledDuringTheWake(t *testing.T) {
	for _, point := range []string{"before host start", "after host start"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			t.Cleanup(func() { killSessions(root) })
			runDaemon(t, root, true, "die-wake+idle+dialog", point, "screen the first run's screen\nreply hello", "reply again").waitKilled(t)
			rec := &screens{}
			st, c, r, err := build(root, options{terminal: true, dialog: wakeDialog,
				wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }}, &syncBuffer{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { r.Close(); st.Close() }()
			if err := r.Reconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := c.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			until(t, "the resumed turn", st, "turn_ended", 2)
			onlyItsOwnScreen(t, rec, 0)
		})
	}
}

// onlyItsOwnScreen checks that of the codecs from the first'th on, exactly
// one was given the screen, and that it holds the woken agent's start
// alone: its dialog first, and nothing of the first run.
func onlyItsOwnScreen(t *testing.T, rec *screens, first int) {
	t.Helper()
	var woken []byte
	rec.mu.Lock()
	n := len(rec.got)
	rec.mu.Unlock()
	for i := first; i < n; i++ {
		if b := rec.of(i); len(b) > 0 {
			if woken != nil {
				t.Fatalf("more than one codec of the woken agent was given the screen")
			}
			woken = b
		}
	}
	if !bytes.HasPrefix(woken, []byte(wakeDialog)) {
		t.Fatalf("the woken codec's screen does not start with its agent's dialog: %q", woken)
	}
	if bytes.Contains(woken, []byte("the first run's screen")) || bytes.Count(woken, []byte("fake: answered")) != 1 {
		t.Fatalf("the woken codec was given the first run's screen: %q", woken)
	}
}

// The same when the woken agent's first output makes the host trim the
// terminal log, which the runs before filled close to its bound: the
// codec is given the new run's screen from its start all the same, and
// answers the dialog (#520).
func TestWokenCodecReadsItsScreenAfterATrim(t *testing.T) {
	rec := &screens{}
	f := newFixture(t, options{terminal: true, dialog: wakeDialog, idlePeriod: 300 * time.Millisecond,
		wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }})
	f.start("screen the first run's screen\nreply hello")
	waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 1) == "asleep" })
	waitFor(t, "the agent's end", 15*time.Second, func() bool { return !agentAlive(t, f.root, 1) })
	// The log a few bytes short of its bound: the new run's dialog passes it.
	p := filepath.Join(StateDir(f.root, 1), host.TerminalLog)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	fill := bytes.Repeat([]byte("earlier output\r\n"), int(host.DefaultLogLimit-st.Size()-10)/16)
	fill = append(fill, bytes.Repeat([]byte("."), int(host.DefaultLogLimit-st.Size()-10)-len(fill))...)
	lf, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lf.Write(fill); err != nil {
		t.Fatal(err)
	}
	lf.Close()

	send(t, owner(f.core), thread, "m2", "reply again")
	waitFor(t, "the resumed turn", 20*time.Second, f.has("turn_ended", 2))
	if st, err := os.Stat(p); err != nil || st.Size() > host.DefaultLogLimit/2+4096 {
		t.Fatalf("the log was not trimmed: %v %v", st.Size(), err)
	}
	onlyItsOwnScreen(t, rec, 1)
}
