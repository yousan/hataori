package codex

import (
	"strings"

	"github.com/yousan/hataori/internal/agent"
)

// flushSteers writes queued inputs into a known, reported running turn.
func (c *codec) flushSteers(a *agent.Actions) bool {
	if !c.ready || c.closed || c.steerBlocked || !c.running || c.turnID == "" || !c.started {
		return false
	}
	for _, s := range c.sent {
		if !s.steer && !s.input.Written {
			return false
		}
	}
	for len(c.queue) > 0 {
		in := c.queue[0]
		c.queue = c.queue[1:]
		id := c.write(a, "turn/steer", map[string]any{"threadId": c.session, "expectedTurnId": c.turnID, "input": []any{map[string]any{"type": "text", "text": in.Text}}}, true)
		a.Inputs = append(a.Inputs, in.ID)
		c.sent = append(c.sent, &pendingInput{input: in, request: id, steer: true})
	}
	return true
}

// inputSteerAccepted reports a successfully delivered input.
func (c *codec) inputSteerAccepted(a *agent.Actions, id int) {
	for i, s := range c.sent {
		if s.steer && s.request == id {
			c.emit(a, agent.Event{Kind: agent.InputTaken, Input: s.input.ID})
			c.sent = append(c.sent[:i], c.sent[i+1:]...)
			return
		}
	}
}

// inputSteerFailed queues a refused input for a later turn.
func (c *codec) inputSteerFailed(a *agent.Actions, id int) {
	for i, s := range c.sent {
		if s.steer && s.request == id {
			c.steerBlocked = c.running
			c.queue = append([]agent.Input{s.input}, c.queue...)
			c.sent = append(c.sent[:i], c.sent[i+1:]...)
			return
		}
	}
}

// matchWrittenSteer recognizes a previously written input after restart.
func (c *codec) matchWrittenSteer(a *agent.Actions, p params) {
	var parts []string
	for _, v := range p.Item.Content {
		parts = append(parts, v.Text)
	}
	for i, s := range c.sent {
		if s.input.Written && normalize(s.input.Text) == normalize(strings.Join(parts, "")) {
			c.emit(a, agent.Event{Kind: agent.InputTaken, Input: s.input.ID})
			c.sent = append(c.sent[:i], c.sent[i+1:]...)
			return
		}
	}
}
