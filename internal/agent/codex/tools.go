package codex

import (
	"path"

	"github.com/yousan/hataori/internal/agent"
)

// toolState tracks tool and subagent lifecycles in start order.
type toolState struct {
	tools     map[string]agent.Event
	toolOrder []string
	subagents map[string]agent.Event
	subOrder  []string
}

// startSubagent reports a subagent once.
func (c *codec) startSubagent(a *agent.Actions, id, name string) {
	if id == "" {
		return
	}
	if c.subagents == nil {
		c.subagents = map[string]agent.Event{}
	}
	if _, ok := c.subagents[id]; ok {
		return
	}
	e := agent.Event{Kind: agent.SubagentStarted, ID: id, Tool: name}
	c.subagents[id] = e
	c.subOrder = append(c.subOrder, id)
	c.emit(a, e)
}

// endSubagent ends a previously reported subagent once.
func (c *codec) endSubagent(a *agent.Actions, id string) {
	e, ok := c.subagents[id]
	if !ok || e.Kind == agent.SubagentEnded {
		return
	}
	e.Kind = agent.SubagentEnded
	c.subagents[id] = e
	c.emit(a, e)
}

// toolItem handles tool and subagent activity items.
func (c *codec) toolItem(a *agent.Actions, p params, method string) bool {
	it := p.Item
	if it.Type == "subAgentActivity" {
		if !c.started && p.ThreadID == c.session {
			c.reportStart(a, agent.ByAgent, -1)
		}
		name := path.Base(it.AgentPath)
		if name == "." || name == "/" {
			name = "subagent"
		}
		if it.Kind == "started" {
			c.startSubagent(a, it.AgentThreadID, name)
		} else if it.Kind == "completed" {
			c.endSubagent(a, it.AgentThreadID)
		}
		return true
	}
	var name string
	switch it.Type {
	case "commandExecution":
		name = "shell"
	case "fileChange":
		name = "apply_patch"
	case "mcpToolCall":
		name = it.Server + "/" + it.Tool
	case "webSearch":
		name = "web_search"
	case "collabAgentToolCall":
		name = it.Tool
	case "sleep":
		name = "sleep"
	default:
		return false
	}
	if method == "item/completed" && c.tools[it.ID].Kind == agent.ToolEnded {
		return true
	}
	if method == "item/completed" && !c.running && p.ThreadID == c.session {
		return true
	}
	if !c.started && p.ThreadID == c.session {
		c.reportStart(a, agent.ByAgent, -1)
	}
	if c.tools == nil {
		c.tools = map[string]agent.Event{}
	}
	if method == "item/started" {
		if _, ok := c.tools[it.ID]; !ok && it.ID != "" {
			parent := ""
			if p.ThreadID != c.session {
				parent = p.ThreadID
			}
			e := agent.Event{Kind: agent.ToolStarted, ID: it.ID, Tool: name, Parent: parent}
			c.tools[it.ID] = e
			c.toolOrder = append(c.toolOrder, it.ID)
			c.emit(a, e)
		}
	} else {
		// A completion without a seen start does not create a tool event.
		if e, ok := c.tools[it.ID]; ok && e.Kind == agent.ToolStarted {
			e.Kind = agent.ToolEnded
			c.tools[it.ID] = e
			c.emit(a, e)
		}
		if it.Type == "collabAgentToolCall" && it.Tool == "spawnAgent" {
			for _, id := range it.ReceiverThreadIDs {
				c.startSubagent(a, id, "subagent")
			}
		}
	}
	return true
}

// endTools closes remaining tools and subagents at the end of the turn.
func (c *codec) endTools(a *agent.Actions) {
	for _, id := range c.toolOrder {
		e := c.tools[id]
		if e.Kind == agent.ToolStarted {
			e.Kind = agent.ToolEnded
			c.tools[id] = e
			c.emit(a, e)
		}
	}
	for _, id := range c.subOrder {
		c.endSubagent(a, id)
	}
	c.toolOrder = nil
	c.subOrder = nil
}
