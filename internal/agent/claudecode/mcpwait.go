package claudecode

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// The wait for MCP servers. The CLI connects to its MCP servers beside its
// start and takes a prompt before they are connected: the turn's first
// request then goes out without their tools (its transcript names them in
// pendingMcpServers), and with 2.1.331 nothing makes an interactive CLI
// wait for them: not alwaysLoad in the MCP config, not
// MCP_CONNECTION_NONBLOCKING=0, and not a UserPromptSubmit hook that waits,
// since the tools are those of the moment the prompt was submitted. So the
// input waits: a server that reports (agent.MCPServer.Reports) hands
// mcpListedEvent to the session host once the CLI has the list of its
// tools, and input is pasted after that. Only the servers hataori gives a
// confined session can report.

// mcpListedEvent is the event of the hook call that a server hands to the
// host.
const mcpListedEvent = "hataori:mcp"

// mcpWait is how long input waits for the MCP servers, counted once, from
// the codec's first Tick after the session's start. After it input is
// pasted all the same, as it was before the codec waited, so that a server
// that never comes holds nothing up for longer.
const mcpWait = 10 * time.Second

// mcpReport is what the server called name hands to the host.
func mcpReport(name string) string {
	b, _ := json.Marshal(map[string]string{"hook_event_name": mcpListedEvent, "server": name})
	return string(b)
}

// mcpState is what the codec knows of the MCP servers it waits for.
type mcpState struct {
	listed []string  // the servers that reported
	since  time.Time // the first Tick after the session's start
	anyway bool      // mcpWait passed: input goes without the servers' word
}

// mcpMissing returns the servers that are still to report.
func (c *codec) mcpMissing() []string {
	if c.spec.Confine == nil {
		return nil
	}
	var missing []string
	for _, n := range c.spec.Confine.Awaits {
		if !slices.Contains(c.mcp.listed, n) {
			missing = append(missing, n)
		}
	}
	return missing
}

// mcpReady reports whether input may be pasted, as far as the MCP servers
// go.
func (c *codec) mcpReady() bool { return c.mcp.anyway || len(c.mcpMissing()) == 0 }

// mcpListed reads a report (payload, of the host's log line): the CLI has
// the tools of the server it names. It writes input that only that held
// back. A report of a server that is not waited for is not known.
func (c *codec) mcpListed(payload, line []byte) Actions {
	var p struct{ Server string }
	if json.Unmarshal(payload, &p) != nil || c.spec.Confine == nil || !slices.Contains(c.spec.Confine.Awaits, p.Server) {
		return c.events(agent.Event{Kind: agent.Unknown, Text: string(line)})
	}
	if !slices.Contains(c.mcp.listed, p.Server) {
		c.mcp.listed = append(c.mcp.listed, p.Server)
	}
	return c.flush()
}

// tickMCP counts mcpWait, and writes input that is still held back once it
// has passed.
func (c *codec) tickMCP(now time.Time) Actions {
	if !c.ready || c.mcpReady() {
		return Actions{}
	}
	if c.mcp.since.IsZero() {
		c.mcp.since = now
		return Actions{}
	}
	if now.Sub(c.mcp.since) < mcpWait {
		return Actions{}
	}
	missing, _ := json.Marshal(c.mcpMissing())
	c.mcp.anyway = true
	return merge(c.events(agent.Event{Kind: agent.Unknown, Text: "no tools after " + mcpWait.String() + " from the MCP servers " + string(missing)}), c.flush())
}
