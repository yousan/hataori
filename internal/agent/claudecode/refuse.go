package claudecode

import "github.com/yousan/hataori/internal/agent"

func (c *codec) RefuseInput(ids []string, reason agent.Reason) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	refused := map[string]bool{}
	for _, id := range ids {
		refused[id] = true
	}
	if c.sent != nil && refused[c.sent.id] {
		c.sent = nil
	}
	queue := c.queue[:0]
	for _, q := range c.queue {
		if !refused[q.id] {
			queue = append(queue, q)
		}
	}
	c.queue = queue
	pasted := c.pasted[:0]
	for _, q := range c.pasted {
		if !refused[q.id] {
			pasted = append(pasted, q)
		}
	}
	c.pasted = pasted
	keep := func(in []string) []string {
		out := in[:0]
		for _, id := range in {
			if !refused[id] {
				out = append(out, id)
			}
		}
		return out
	}
	c.leftover, c.unconfirmed = keep(c.leftover), keep(c.unconfirmed)
	var events []agent.Event
	for _, id := range ids {
		events = append(events, agent.Event{Kind: agent.TurnEnded, Input: id, Status: agent.Failed, Reason: reason})
	}
	return c.events(events...)
}
