package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/holder"
)

// refuser is an adapter whose codecs hand their input over as keys that can
// fail, and write down what they are told.
type refuser struct {
	agent.Adapter
	mu  sync.Mutex
	got []bool
}

type refusedCodec struct {
	agent.Codec
	h *refuser
}

func (h *refuser) NewCodec(s agent.Spec) agent.Codec {
	return &refusedCodec{Codec: h.Adapter.NewCodec(s), h: h}
}

func (h *refuser) told() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool{}, h.got...)
}

func (c *refusedCodec) Input(in agent.Input) (agent.Actions, error) {
	a, err := c.Codec.Input(in)
	if in.Written || in.Text != "refuse this" {
		return a, err
	}
	// A key that the host cannot encode: nothing is handed over.
	a.Keys = []agent.Part{{Paste: "x", Key: agent.Enter}}
	a.KeysFailed = func(nothing bool) {
		c.h.mu.Lock()
		defer c.h.mu.Unlock()
		c.h.got = append(c.h.got, nothing)
	}
	return a, err
}

// Keys that the holder did not take are reported to the codec at once, the
// input they carried is recorded as not written, so that a restart writes
// it, and the send is no error: the codec says with the turn's end what
// became of it (#741).
func TestKeysNotHandedOverAreReported(t *testing.T) {
	h := &refuser{}
	f := newFixture(t, options{terminal: true, inputTimeout: "60s", wrap: func(a agent.Adapter) agent.Adapter { h.Adapter = a; return h }})
	f.start("reply hello")
	waitFor(t, "the turn's end", 15*time.Second, f.has("turn_ended", 1))
	send(t, owner(f.core), thread, "k2", "refuse this")
	if got := h.told(); len(got) != 1 || !got[0] {
		t.Fatalf("the codec was told %v, want once that nothing was handed over", got)
	}
	var written bool
	if err := f.st.DB.QueryRowContext(context.Background(), `SELECT written FROM pending_inputs`).Scan(&written); err != nil || written {
		t.Fatalf("the input is recorded as written %v, %v", written, err)
	}
	lv, err := f.run.get(1)
	if err != nil {
		t.Fatal(err)
	}
	lv.mu.Lock()
	defer lv.mu.Unlock()
	var no *holder.NotSent
	if err := f.run.do(lv, agent.Actions{Keys: []agent.Part{{Paste: "x", Key: agent.Enter}}}); !errors.As(err, &no) {
		t.Fatalf("keys without KeysFailed: %v", err)
	}
}

// early is an adapter whose codecs have nothing to interrupt yet.
type early struct{ agent.Adapter }

type earlyCodec struct{ agent.Codec }

func (h early) NewCodec(s agent.Spec) agent.Codec { return earlyCodec{h.Adapter.NewCodec(s)} }

func (earlyCodec) Interrupt() agent.Actions { return agent.Actions{NotInterrupted: true} }

// An interrupt that the codec cannot carry out yet is none: the Stop says
// so, the turn goes on after the grace time and names nobody (#741).
func TestAnInterruptThatIsNone(t *testing.T) {
	grace := 200 * time.Millisecond
	f := newFixture(t, options{interrupt: grace, wrap: func(a agent.Adapter) agent.Adapter { return early{a} }})
	f.start("tool wait 5s\nreply late")
	waitFor(t, "the tool", 15*time.Second, f.has("tool_started", 1))
	if err := owner(f.core).Stop(context.Background(), thread, 0); !errors.Is(err, control.ErrNotInterrupted) {
		t.Fatalf("the stop: %v", err)
	}
	waitFor(t, "the turn's end", 20*time.Second, f.has("turn_ended", 1))
	if d := f.eventData("turn_ended")[0]; d["outcome"] != "completion" {
		t.Errorf("the turn's end: %v", d)
	}
	var by string
	if err := f.st.DB.QueryRow(`SELECT stopped_by FROM turns`).Scan(&by); err != nil || by != "" {
		t.Errorf("stopped_by %q, %v", by, err)
	}
}
