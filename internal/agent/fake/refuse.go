package fake

import "github.com/yousan/hataori/internal/agent"

func (c *codec) RefuseInput(ids []string, reason agent.Reason) agent.Actions {
	c.mu.Lock()
	defer c.mu.Unlock()
	refused := map[string]bool{}
	for _, id := range ids {
		refused[id] = true
	}
	waiting := c.waiting[:0]
	for _, w := range c.waiting {
		if !refused[w.id] {
			waiting = append(waiting, w)
		}
	}
	c.waiting = waiting
	queue := c.queue[:0]
	for _, in := range c.queue {
		if !refused[in.ID] {
			queue = append(queue, in)
		}
	}
	c.queue = queue
	var events []agent.Event
	for _, id := range ids {
		events = append(events, agent.Event{Kind: agent.TurnEnded, Input: id, Status: agent.Failed, Reason: reason})
	}
	return c.events(events...)
}
