package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/session"
)

// MCP is `hataori mcp`: an MCP server on stdio whose tools are the control
// API calls of section 7.1, made on one connection bound to a supervisor's
// Caller. It decides nothing about who may do what: the daemon bound the
// connection, and the core checks every call. A changed MCP server gets no
// more than the connection allows.
type MCP struct {
	// Dial connects to the daemon. It is called on the first tool call, so
	// that the server answers initialize and tools/list without a daemon.
	Dial func() (control.API, error)
	// Scope is the scope the connection was issued for: where start_thread
	// starts, and which threads the tools name by number.
	Scope string
	// OutputMax is the most bytes of a thread's output read_thread returns.
	OutputMax int
	// Version is hataori's version, for serverInfo.
	Version string

	mu  sync.Mutex
	api control.API
}

// DefaultOutputMax is OutputMax when the MCP config names none.
const DefaultOutputMax = 4096

// The tools, in the order tools/list gives them.
const (
	ToolList      = "list_threads"
	ToolRead      = "read_thread"
	ToolSend      = "send_to_thread"
	ToolInterrupt = "interrupt_thread"
	ToolStart     = "start_thread"
)

// protocols are the MCP versions the server speaks, newest first.
var protocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes.
const (
	parseError     = -32700
	methodNotFound = -32601
	invalidParams  = -32602
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var number = map[string]any{"type": "integer", "description": "the thread's number, as list_threads shows it"}

// tools is what tools/list returns.
var tools = []tool{
	{ToolList, "List the threads of this channel: number, state, title, who started it, and the last line of output.",
		object(map[string]any{})},
	{ToolRead, "Read one thread of this channel: its state, its open question if any, and the end of its latest output.",
		object(map[string]any{"number": number}, "number")},
	{ToolSend, "Send a message to a thread of this channel. A thread that is running a turn refuses it with \"busy\"; " +
		"you are told when its turn ends, and can send then. A thread waiting on a question takes the message as the answer.",
		object(map[string]any{"number": number, "text": map[string]any{"type": "string", "description": "the message"}}, "number", "text")},
	{ToolInterrupt, "Ask a person in this channel to approve interrupting a thread that is running a turn. This returns at once and interrupts nothing by itself. Once approved, the thread's turn is stopped and it gets the reason as a message from the supervisor. A thread not running is refused with not busy; send instead.", object(map[string]any{"number": number, "reason": map[string]any{"type": "string", "description": "the reason to give the thread"}}, "number", "reason")},
	{ToolStart, "Start a new thread in this channel, with its own workspace cloned from a repository.",
		object(map[string]any{
			"repo":   map[string]any{"type": "string", "description": "the repository to clone"},
			"text":   map[string]any{"type": "string", "description": "the first message; its first line becomes the title"},
			"model":  map[string]any{"type": "string", "description": "the model; leave out for the channel's default"},
			"effort": map[string]any{"type": "string", "description": "the effort; leave out for the channel's default", "enum": control.Efforts},
		}, "repo", "text")},
}

// Serve answers MCP requests read from r, one JSON-RPC message per line,
// writing to w, until r ends or ctx is done.
func (m *MCP) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{parseError, "not a JSON-RPC message"}}); err != nil {
				return err
			}
			continue
		}
		if len(msg.ID) == 0 || msg.Method == "" {
			continue // a notification, or a response to nothing we asked
		}
		result, rerr := m.handle(ctx, msg)
		if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: msg.ID, Result: result, Error: rerr}); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (m *MCP) handle(ctx context.Context, msg rpcMessage) (any, *rpcError) {
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(msg.Params, &p)
		version := protocols[0]
		for _, v := range protocols {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "hataori", "version": m.Version},
			"instructions": "Tools for the threads of this channel. You can list, read, start and send; " +
				"you can ask a person to approve interrupting a running turn. Your messages reach a thread marked as the supervisor's.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, &rpcError{invalidParams, "bad tools/call parameters"}
		}
		text, err := m.call(ctx, p.Name, p.Arguments)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{methodNotFound, "method not found: " + msg.Method}
}

func toolResult(text string, isError bool) map[string]any {
	r := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if isError {
		r["isError"] = true
	}
	return r
}

// connect returns the daemon's API, dialing it the first time.
func (m *MCP) connect() (control.API, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.api != nil {
		return m.api, nil
	}
	api, err := m.Dial()
	if err != nil {
		return nil, fmt.Errorf("cannot reach hataori: %v", err)
	}
	m.api = api
	return api, nil
}

// call runs one tool and returns its answer, or an error whose text names
// the reason for the model.
func (m *MCP) call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var a struct {
		Number *int64 `json:"number"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
		Repo   string `json:"repo"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	switch name {
	case ToolList, ToolRead, ToolSend, ToolInterrupt, ToolStart:
	default:
		return "", fmt.Errorf("no tool %q", name)
	}
	api, err := m.connect()
	if err != nil {
		return "", err
	}
	switch name {
	case ToolList:
		ts, err := api.Threads(ctx, control.Filter{Scope: m.Scope})
		if err != nil {
			return "", reason(err)
		}
		return listing(ts), nil
	case ToolStart:
		if a.Repo == "" || strings.TrimSpace(a.Text) == "" {
			return "", errors.New("bad arguments: repo and text are required")
		}
		id, err := api.Start(ctx, control.StartRequest{Key: newKey(), Scope: m.Scope, Repo: a.Repo,
			Input: control.Input{Text: a.Text}, Model: a.Model, Effort: a.Effort})
		if err != nil {
			return "", reason(err)
		}
		return fmt.Sprintf("Started thread #%d.", id), nil
	}
	if a.Number == nil {
		return "", errors.New("bad arguments: number is required")
	}
	id, err := m.find(ctx, api, *a.Number)
	if err != nil {
		return "", err
	}
	if name == ToolRead {
		d, err := api.Thread(ctx, id, m.outputMax()+1, 0)
		if err != nil {
			return "", reason(err)
		}
		return detail(d, m.outputMax()), nil
	}
	if name == ToolInterrupt {
		if strings.TrimSpace(a.Reason) == "" {
			return "", errors.New("bad arguments: reason is required")
		}
		if _, err := api.AskInterrupt(ctx, id, newKey(), a.Reason); err != nil {
			if errors.Is(err, session.ErrRefused) {
				return "", fmt.Errorf("not busy: %v", err)
			}
			return "", reason(err)
		}
		return "asked a person: nothing is interrupted until a person in this channel approves it. An approved interrupt ends the worker's turn, and that turn end wakes the supervisor as any other; a refusal or an approval after the turn ended is said in the supervisor's chat thread.", nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return "", errors.New("bad arguments: text is required")
	}
	// Refuse is all this caller can ask for; the daemon would refuse
	// anything else the same way.
	if err := api.Send(ctx, id, newKey(), control.Input{Text: a.Text}, control.Refuse); err != nil {
		return "", reason(err)
	}
	return fmt.Sprintf("Sent to thread #%d.", *a.Number), nil
}

func (m *MCP) outputMax() int {
	if m.OutputMax > 0 {
		return m.OutputMax
	}
	return DefaultOutputMax
}

// find returns the thread of the scope with number n.
func (m *MCP) find(ctx context.Context, api control.API, n int64) (control.ThreadID, error) {
	ts, err := api.Threads(ctx, control.Filter{Scope: m.Scope, Thread: control.ThreadID(n)})
	if err != nil && !errors.Is(err, control.ErrForbidden) { // forbidden: the number is another scope's
		return 0, reason(err)
	}
	if len(ts) == 0 {
		return 0, fmt.Errorf("not in this scope: no thread #%d in this channel", n)
	}
	return ts[0].ID, nil
}

// reason turns an error of the control API into what the model reads:
// first the reason, as one of "busy", "not in this scope" and "not
// allowed", then the details.
func reason(err error) error {
	switch {
	case errors.Is(err, core.ErrBusy):
		return fmt.Errorf("busy: the thread is running a turn; nothing was sent. You are told when the turn ends")
	case errors.Is(err, core.ErrNotFound):
		return fmt.Errorf("not in this scope: no such thread in this channel")
	case errors.Is(err, control.ErrForbidden):
		return fmt.Errorf("not allowed: %v", err)
	case errors.Is(err, core.ErrDisabled):
		return fmt.Errorf("not allowed: the channel is not enabled")
	}
	return fmt.Errorf("failed: %v", err)
}

func newKey() string {
	var b [16]byte
	rand.Read(b[:])
	return "supervisor-" + hex.EncodeToString(b[:])
}

func listing(ts []control.ThreadInfo) string {
	if len(ts) == 0 {
		return "No threads in this channel."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d threads in this channel:\n", len(ts))
	for _, t := range ts {
		fmt.Fprintf(&b, "#%d %s %q, started by %s, updated %s", t.ID, t.State, t.Title, t.StartedBy, t.Updated.UTC().Format("2006-01-02 15:04Z"))
		if t.Last != "" {
			fmt.Fprintf(&b, "; last: %s", t.Last)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func detail(d control.ThreadDetail, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Thread #%d: %s\nState: %s, since %s\nStarted by: %s\nRepository: %s\n", d.ID, d.Title, d.State,
		d.Updated.UTC().Format("2006-01-02 15:04Z"), d.StartedBy, d.Repo)
	for _, r := range d.Requests {
		kind := "Open question"
		if r.Kind == control.Approval {
			kind = "Open approval"
		}
		fmt.Fprintf(&b, "%s: %s\n", kind, r.Text)
		for _, c := range r.Choices {
			fmt.Fprintf(&b, "  choice: %s\n", c.Label)
		}
	}
	out := d.Recent
	if len(out) > max {
		cut := len(out) - max
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
		out = out[cut:]
		fmt.Fprintf(&b, "Latest output, cut: only its last %d bytes are shown.\n", max)
	} else {
		b.WriteString("Latest output:\n")
	}
	if out == "" {
		out = "(none)"
	}
	b.WriteString(out)
	return b.String()
}
