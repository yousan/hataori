package claudecode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// The transcripts' sign of a turn without its MCP servers' tools, and the
// order in which the CLI does things, were seen on 2.1.331 (#704). A report
// is not the CLI's: it is what hataori's own MCP server hands to the host.

// awaiting makes c the codec of a confined session whose server "hataori"
// reports, as the runner's stored spec says.
func awaiting(c *codec) *codec {
	c.spec.Confine = &agent.Confinement{Awaits: []string{"hataori"}}
	return c
}

// report is the host's log line of the report of the server called name.
func report(id, name string) []byte {
	b, _ := json.Marshal(map[string]any{"hook": id, "wait": false, "payload": json.RawMessage(mcpReport(name))})
	return b
}

// boxUp is the CLI's input box coming up.
func boxUp(t *testing.T) []byte { return readScreen(t, firstEnter+"screen-after.raw") }

// The first input waits until the server reported, also with the input box
// up for long before.
func TestFirstInputWaitsForTheMCPServer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reportedFirst bool // before the box is up
	}{{"box first", false}, {"report first", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c := awaiting(firstStart(t))
			now := time.Now()
			c.Tick(now)
			if a, _ := c.Input(agent.Input{ID: "first", Text: firstText}); len(a.Keys) != 0 {
				t.Fatalf("written %q at the start", a.Keys)
			}
			steps := []func() Actions{
				func() Actions { return c.Decode(agent.Screen, boxUp(t)) },
				func() Actions { return c.Decode(agent.Hook, report("m1", "hataori")) },
			}
			if tc.reportedFirst {
				steps[0], steps[1] = steps[1], steps[0]
			}
			a := steps[0]()
			now = now.Add(mcpWait - time.Second)
			a = merge(a, c.Tick(now))
			if len(a.Keys) != 0 || len(a.Events) != 0 {
				t.Fatalf("writes %q and events %+v after the first of the two", a.Keys, a.Events)
			}
			a = steps[1]()
			if len(a.Keys) != 1 || a.Keys[0] != (agent.Part{Paste: firstText}) || len(a.Inputs) != 1 {
				t.Fatalf("writes %q, inputs %q after both, want the paste", a.Keys, a.Inputs)
			}
			if a := c.Tick(now.Add(time.Minute)); len(a.Events) != 0 {
				t.Fatalf("events %+v though the server reported", a.Events)
			}
		})
	}
}

// Another server's report is not the awaited one's, and one that nothing
// awaits is not known.
func TestAnotherServersReportDoesNotCount(t *testing.T) {
	c := awaiting(firstStart(t))
	c.Input(agent.Input{Text: firstText})
	c.Decode(agent.Screen, boxUp(t))
	unconfined := firstStart(t)
	for _, a := range []Actions{c.Decode(agent.Hook, report("m1", "other")), unconfined.Decode(agent.Hook, report("m1", "hataori"))} {
		if len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Kind != agent.Unknown {
			t.Fatalf("written %q, events %+v", a.Keys, a.Events)
		}
	}
	if len(c.mcp.listed) != 0 || len(unconfined.mcp.listed) != 0 {
		t.Fatalf("kept %q and %q", c.mcp.listed, unconfined.mcp.listed)
	}
}

// A field of a hook call of the CLI's own that has the report's name is not
// read: the call stays what it is.
func TestHookCallWithAServerField(t *testing.T) {
	c := (&Adapter{}).NewCodec(agent.Spec{Dir: "/tmp/capture-work/sessions/first_enter_lost"}).(*codec)
	start := bytes.TrimSpace(readScreen(t, firstEnter+"hooks/01-SessionStart.json"))
	start = append([]byte(`{"server":{"a":1},`), start[1:]...)
	a := c.Decode(agent.Hook, envelope("s1", "first_enter_lost/hooks/01-SessionStart.json", start))
	if len(a.Events) != 1 || a.Events[0].Kind != agent.SessionReady {
		t.Fatalf("events %+v, want the start", a.Events)
	}
}

// An interrupt while the input is held back ends its turn and drops it: no
// key is pressed, and neither the report nor the deadline writes it. What
// came after it goes as it would have.
func TestInterruptOfHeldInput(t *testing.T) {
	for _, after := range []string{"the report", "the deadline"} {
		c := awaiting(firstStart(t))
		c.Decode(agent.Screen, boxUp(t))
		now := time.Now()
		c.Tick(now)
		c.Input(agent.Input{ID: "first", Text: firstText})
		c.Input(agent.Input{ID: "2", Text: "and then"})
		a := c.Interrupt()
		if len(a.Keys) != 0 || len(a.Events) != 1 {
			t.Fatalf("pressed %q, events %+v, want the turn's end alone", a.Keys, a.Events)
		}
		if e := a.Events[0]; e.Kind != agent.TurnEnded || e.Status != agent.Interrupted || e.Input != "first" || e.Seq == 0 {
			t.Fatalf("event %+v, want the turn of the first input interrupted", e)
		}
		if after == "the report" {
			a = c.Decode(agent.Hook, report("m1", "hataori"))
		} else {
			a = c.Tick(now.Add(mcpWait))
		}
		if len(a.Keys) != 1 || a.Keys[0] != (agent.Part{Paste: "and then"}) {
			t.Fatalf("after %s: writes %q, want the second input", after, a.Keys)
		}
		if a := c.Interrupt(); len(a.Keys) != 1 || len(a.Events) != 0 {
			t.Fatalf("after %s: an interrupt of written input presses %q, events %+v", after, a.Keys, a.Events)
		}
	}
}

// A server that never reports holds input up for mcpWait, once: with or
// without input by then, what comes after goes at once.
func TestInputGoesAfterMCPWait(t *testing.T) {
	for _, early := range []bool{true, false} {
		c := awaiting(firstStart(t))
		c.Decode(agent.Screen, boxUp(t))
		if early {
			c.Input(agent.Input{Text: firstText})
		}
		start := time.Now()
		var events []agent.Event
		keys := 0
		for d := time.Duration(0); d < mcpWait; d += time.Second {
			a := c.Tick(start.Add(d))
			keys += len(a.Keys)
			events = append(events, a.Events...)
		}
		if keys != 0 || len(events) != 0 {
			t.Fatalf("early %t: %d keys, events %+v before mcpWait", early, keys, events)
		}
		a := c.Tick(start.Add(mcpWait))
		if len(a.Events) != 1 || a.Events[0].Kind != agent.Unknown || !strings.Contains(a.Events[0].Text, "hataori") {
			t.Fatalf("early %t: events %+v at mcpWait, want one that names the server", early, a.Events)
		}
		if !early {
			if a, _ = c.Input(agent.Input{Text: firstText}); len(a.Events) != 0 {
				t.Fatalf("events %+v", a.Events)
			}
		}
		if len(a.Keys) != 1 || a.Keys[0] != (agent.Part{Paste: firstText}) {
			t.Fatalf("early %t: writes %q after mcpWait, want the paste", early, a.Keys)
		}
		for i := 1; i < 20; i++ {
			if a := c.Tick(start.Add(mcpWait + time.Duration(i)*time.Second)); len(a.Events) != 0 {
				t.Fatalf("early %t: events %+v again", early, a.Events)
			}
		}
	}
}

// A session that awaits no server, as a thread's that is not confined, does
// not wait.
func TestNoWaitWithoutAnAwaitedServer(t *testing.T) {
	for _, confine := range []*agent.Confinement{nil, {}} {
		c := firstStart(t)
		c.spec.Confine = confine
		c.Input(agent.Input{Text: firstText})
		if a := c.Decode(agent.Screen, boxUp(t)); len(a.Keys) != 1 {
			t.Fatalf("writes %q with the box up, want the paste", a.Keys)
		}
	}
}

// A codec rebuilt after a restart knows the report from the log's replay,
// read before it or after the input is handed over.
func TestRebuiltCodecKnowsTheReport(t *testing.T) {
	for _, read := range []bool{true, false} {
		c := awaiting((&Adapter{}).NewCodec(agent.Spec{Dir: "/tmp/capture-work/sessions/first_enter_lost"}).(*codec))
		start := readScreen(t, firstEnter+"hooks/01-SessionStart.json")
		c.Decode(agent.Hook, envelope("s1", "first_enter_lost/hooks/01-SessionStart.json", start))
		if read {
			c.Decode(agent.Hook, report("m1", "hataori"))
		}
		a, _ := c.Input(agent.Input{Text: firstText})
		if read != (len(a.Keys) == 1) {
			t.Fatalf("the report read %t: the input writes %q", read, a.Keys)
		}
		if !read {
			a = c.Decode(agent.Hook, report("m1", "hataori"))
		}
		if len(a.Keys) != 1 {
			t.Fatalf("the report read %t: writes %q, want the paste", read, a.Keys)
		}
	}
}

// The CLI may start its servers while a start-up dialog is still up: a
// report does not tell that the dialog is gone.
func TestAReportIsNotTheStart(t *testing.T) {
	c := awaiting(screenCodec("/tmp/capture-work"))
	c.Decode(agent.Hook, report("m1", "hataori"))
	if c.started || c.ready {
		t.Fatalf("started %t, ready %t after a report", c.started, c.ready)
	}
}
