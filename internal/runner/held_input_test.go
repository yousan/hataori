package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/session"
)

// withholder is an adapter whose codecs hold input back until release, as one
// that waits for the agent to be able to take it, and drop the oldest they
// hold at an interrupt.
type withholder struct {
	agent.Adapter
	mu       sync.Mutex
	released bool
}

func (h *withholder) release(on bool) {
	h.mu.Lock()
	h.released = on
	h.mu.Unlock()
}

type holdingCodec struct {
	agent.Codec
	h    *withholder
	held []agent.Input // under h.mu
}

func (h *withholder) NewCodec(s agent.Spec) agent.Codec {
	return &holdingCodec{Codec: h.Adapter.NewCodec(s), h: h}
}

func (c *holdingCodec) Input(in agent.Input) (agent.Actions, error) {
	if in.Written {
		return c.Codec.Input(in)
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	c.held = append(c.held, in)
	return agent.Actions{}, nil
}

func (c *holdingCodec) Interrupt() agent.Actions {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if len(c.held) == 0 {
		return c.Codec.Interrupt()
	}
	in := c.held[0]
	c.held = c.held[1:]
	return agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Status: agent.Interrupted, Input: in.ID}}}
}

func (c *holdingCodec) Tick(now time.Time) agent.Actions {
	a := c.Codec.Tick(now)
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if c.h.released {
		for _, in := range c.held {
			b, _ := c.Codec.Input(in)
			a.Events = append(a.Events, b.Events...)
			a.Write = append(a.Write, b.Write...)
			a.Inputs = append(a.Inputs, b.Inputs...)
		}
		c.held = nil
	}
	return a
}

// A Stop of a turn whose input the codec still holds back ends the turn:
// the input is not delivered later, and the session is not stopped for a
// turn that does not end (#704).
func TestStopOfAnInputTheCodecHolds(t *testing.T) {
	h := &withholder{}
	grace := time.Second
	f := newFixture(t, options{interrupt: grace, wrap: func(a agent.Adapter) agent.Adapter { h.Adapter = a; return h }})
	stop := func() {
		t.Helper()
		if err := f.run.Interrupt(context.Background(), 1, 0, session.Stopper{}); err != nil {
			t.Fatal(err)
		}
	}
	f.start("reply first")
	waitFor(t, "the turn's start", 15*time.Second, f.has("turn_started", 1))
	stop()
	var fi string
	f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = 1`).Scan(&fi)
	if !f.has("turn_ended", 1)() || sessionState(t, f.st, 1) != "idle" || fi != "" {
		t.Fatalf("after the stop: turns %v, %s, first input %q", f.eventData("turn_ended"), sessionState(t, f.st, 1), fi)
	}
	// And one that was sent to the session as it runs.
	send(t, owner(f.core), thread, "m2", "reply second")
	waitFor(t, "the second turn's start", 15*time.Second, f.has("turn_started", 2))
	stop()
	var rows int
	f.st.DB.QueryRow(`SELECT count(*) FROM pending_inputs`).Scan(&rows)
	if !f.has("turn_ended", 2)() || sessionState(t, f.st, 1) != "idle" || rows != 0 {
		t.Fatalf("after the second stop: turns %v, %s, %d pending", f.eventData("turn_ended"), sessionState(t, f.st, 1), rows)
	}
	for _, e := range f.eventData("turn_ended") {
		if e["reason"] != session.ReasonInterrupted {
			t.Fatalf("a stopped turn ended as %v", e)
		}
	}
	h.release(true)
	send(t, owner(f.core), thread, "m3", "reply third")
	waitFor(t, "the third turn's end", 15*time.Second, f.has("turn_ended", 3))
	time.Sleep(grace + 500*time.Millisecond) // past the watch of the interrupts
	texts := f.eventData("text")
	if len(texts) != 1 || texts[0]["text"] != "third" || sessionState(t, f.st, 1) != "idle" || strings.Contains(f.log.String(), "stopping the session") {
		t.Fatalf("texts %v, %s, want the third answer alone and the session up\n%s", texts, sessionState(t, f.st, 1), f.log.String())
	}
}

// A first input that the codec still held back when a commit failed is held
// by the codec built again, and delivered once.
func TestHeldFirstInputSurvivesAFailedCommit(t *testing.T) {
	h := &withholder{}
	f := newFixture(t, options{wrap: func(a agent.Adapter) agent.Adapter { h.Adapter = a; return h }})
	f.start("reply hello")
	waitFor(t, "the turn's start", 15*time.Second, f.has("turn_started", 1))
	if _, err := f.st.DB.Exec(`CREATE TRIGGER fail_read BEFORE UPDATE OF log_offset ON sessions BEGIN SELECT RAISE(ABORT, 'test: not read'); END`); err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(filepath.Join(StateDir(f.root, 1), host.EventsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	lf.WriteString(`{"t":"file","path":"/tmp/x"}` + "\n")
	lf.Close()
	waitFor(t, "a failed commit", 15*time.Second, func() bool { return strings.Contains(f.log.String(), "test: not read") })
	if _, err := f.st.DB.Exec(`DROP TRIGGER fail_read`); err != nil {
		t.Fatal(err)
	}
	h.release(true)
	waitFor(t, "the turn's end", 15*time.Second, f.has("turn_ended", 1))
	if texts := f.eventData("text"); len(texts) != 1 || texts[0]["text"] != "hello" {
		t.Fatalf("texts %v", texts)
	}
}

// What a codec reports outside of reading a line is in the daemon's log, and
// is no event.
func TestCodecReportIsLogged(t *testing.T) {
	h := &withholder{}
	f := newFixture(t, options{wrap: func(a agent.Adapter) agent.Adapter { h.Adapter = a; return h }})
	h.release(true)
	f.start("reply hello")
	waitFor(t, "the turn's end", 15*time.Second, f.has("turn_ended", 1))
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	lv.mu.Lock()
	f.run.do(lv, agent.Actions{Events: []agent.Event{{Kind: agent.Unknown, Text: "no tools after 10s"}}})
	lv.mu.Unlock()
	if !strings.Contains(f.log.String(), "no tools after 10s") {
		t.Fatalf("not in the log:\n%s", f.log.String())
	}
}
