package fake

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/yousan/hataori/internal/agent"
)

// mcpClient is the program's client of the first MCP server of a confined
// spec, as an agent's: the server is started on the first call, on stdio,
// and kept until the program exits, so that it outlives a daemon's restart
// as an agent's MCP server does.
type mcpClient struct {
	config string // the MCPConfigFile; "" when the spec was not confined

	mu  sync.Mutex
	in  io.WriteCloser
	out *bufio.Scanner
	id  int
}

// call calls tool with args (a JSON object, or empty) and returns the text
// of its result, and whether the server marked it an error.
func (m *mcpClient) call(tool string, args string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == "" {
		return "", false, errors.New("no MCP server: the session is not confined")
	}
	if m.in == nil {
		if err := m.start(); err != nil {
			return "", false, err
		}
	}
	params := map[string]any{"name": tool}
	if strings.TrimSpace(args) != "" {
		params["arguments"] = json.RawMessage(args)
	}
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	if err := m.request("tools/call", params, &res); err != nil {
		return "", false, err
	}
	var texts []string
	for _, c := range res.Content {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n"), res.IsError, nil
}

// start starts the server and initializes it; m.mu is held.
func (m *mcpClient) start() error {
	b, err := os.ReadFile(m.config)
	if err != nil {
		return err
	}
	var servers []agent.MCPServer
	if err := json.Unmarshal(b, &servers); err != nil {
		return err
	}
	if len(servers) == 0 || len(servers[0].Argv) == 0 {
		return errors.New("no MCP server in the config")
	}
	s := servers[0]
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Env = append(os.Environ(), s.Env...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	m.in, m.out = in, bufio.NewScanner(out)
	m.out.Buffer(make([]byte, 64<<10), 16<<20)
	if err := m.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "fake", "version": "0"}}, nil); err != nil {
		return err
	}
	return m.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (m *mcpClient) send(v any) error {
	b, _ := json.Marshal(v)
	_, err := m.in.Write(append(b, '\n'))
	return err
}

// request sends one request and reads lines until its response.
func (m *mcpClient) request(method string, params any, result any) error {
	m.id++
	if err := m.send(map[string]any{"jsonrpc": "2.0", "id": m.id, "method": method, "params": params}); err != nil {
		return err
	}
	for m.out.Scan() {
		var resp struct {
			ID     int
			Result json.RawMessage
			Error  *struct{ Message string }
		}
		if json.Unmarshal(m.out.Bytes(), &resp) != nil || resp.ID != m.id {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, result)
	}
	return fmt.Errorf("%s: the MCP server ended: %v", method, m.out.Err())
}
