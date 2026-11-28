package codex

import "github.com/yousan/hataori/internal/agent"

func (c *codec) RefuseInput(ids []string, reason agent.Reason) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	refused := map[string]bool{}
	for _, id := range ids {
		refused[id] = true
	}
	var a agent.Actions
	kept := c.sent[:0]
	for _, s := range c.sent {
		if !refused[s.input.ID] {
			kept = append(kept, s)
			continue
		}
		delete(c.requests, s.request)
		delete(c.steers, s.request)
		if !s.steer {
			c.running = s.input.IdleOnly && c.nativeTurn != ""
		}
	}
	c.sent = kept
	queue := c.queue[:0]
	for _, in := range c.queue {
		if !refused[in.ID] {
			queue = append(queue, in)
		}
	}
	c.queue = queue
	for _, id := range ids {
		c.emit(&a, agent.Event{Kind: agent.TurnEnded, Input: id, Status: agent.Failed, Reason: reason})
	}
	return a
}
