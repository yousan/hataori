package codex

import (
	"encoding/json"
	"errors"

	"github.com/yousan/hataori/internal/agent"
)

func (*Adapter) CanDeferInput() bool { return true }

func deferredCommand(command [][]byte) (id int, thread string, err error) {
	if len(command) != 1 {
		return 0, "", errors.New("codex: deferred input needs one complete turn/start")
	}
	var m struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Thread string `json:"threadId"`
		} `json:"params"`
	}
	if json.Unmarshal(command[0], &m) != nil || m.ID <= 0 || m.Method != "turn/start" || m.Params.Thread == "" {
		return 0, "", errors.New("codex: deferred input is not an identified turn/start")
	}
	return m.ID, m.Params.Thread, nil
}

func (*Adapter) ValidateDeferredInput(a agent.Actions) error {
	if len(a.Inputs) != 1 || len(a.Keys) != 0 || len(a.Reply) != 0 {
		return errors.New("codex: deferred input is not a single pipe submission")
	}
	_, _, err := deferredCommand(a.Write)
	return err
}

type deferredReply struct {
	turn     string
	err      bool
	finished bool
}

func (c *codec) rememberDeferred(id int, r deferredReply) {
	if c.deferredReplies == nil {
		c.deferredReplies = make(map[int]deferredReply)
	}
	// A missing old receipt is never a grant to infer delivery from text.
	// Live matching inputs are confirmed immediately; this bounded cache
	// handles responses encountered before Written input during warmIO.
	if len(c.deferredReplies) >= 128 {
		oldest := id
		for n := range c.deferredReplies {
			oldest = min(oldest, n)
		}
		delete(c.deferredReplies, oldest)
	}
	r.finished = c.deferredCompleted[r.turn]
	c.deferredReplies[id] = r
}

// restoreDeferred restores the exact original request, never a text match.
// Responses read during warmIO are cached before Written input is restored.
func (c *codec) restoreDeferred(in agent.Input) (agent.Actions, error) {
	var a agent.Actions
	id, thread, err := deferredCommand(in.Command)
	if err != nil || c.session != "" && c.session != thread {
		return a, errors.New("codex: missing or changed deferred request correlation")
	}
	if method := c.requests[id]; method != "" && method != "turn/start" {
		return a, errors.New("codex: deferred request identity is already used")
	}
	c.next = max(c.next, id+1)
	c.requests[id] = "turn/start"
	if reply, ok := c.deferredReplies[id]; !ok || (!reply.err && !reply.finished) {
		c.running = true
	}
	for _, s := range c.sent {
		if s.input.ID == in.ID && s.request == id {
			s.input = in
			s.at = in.WrittenAt
			c.confirmDeferred(&a, id)
			return a, nil
		}
	}
	c.sent = append(c.sent, &pendingInput{input: in, request: id, thread: thread, at: in.WrittenAt})
	c.confirmDeferred(&a, id)
	return a, nil
}

func (c *codec) confirmDeferred(a *agent.Actions, id int) {
	r, ok := c.deferredReplies[id]
	if !ok {
		return
	}
	for i, s := range c.sent {
		if !s.input.IdleOnly || s.request != id || s.thread != c.session {
			continue
		}
		if r.err {
			c.emit(a, agent.Event{Kind: agent.TurnEnded, Input: s.input.ID, Status: agent.Failed, Reason: agent.NotDelivered})
		} else if r.turn != "" {
			c.emit(a, agent.Event{Kind: agent.InputTaken, Input: s.input.ID})
		} else {
			return
		}
		c.sent = append(c.sent[:i], c.sent[i+1:]...)
		return
	}
}

func (c *codec) completeDeferredTurn(turn string) {
	if turn == "" {
		return
	}
	if c.deferredCompleted == nil {
		c.deferredCompleted = map[string]bool{}
	}
	if len(c.deferredCompleted) >= 128 {
		for old := range c.deferredCompleted {
			delete(c.deferredCompleted, old)
			break
		}
	}
	c.deferredCompleted[turn] = true
	for id, r := range c.deferredReplies {
		if r.turn == turn {
			r.finished = true
			c.deferredReplies[id] = r
		}
	}
}
