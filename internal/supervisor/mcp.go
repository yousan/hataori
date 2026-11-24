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
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
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
	// Listed, when set, is called once, after the first answer to
	// tools/list was written: the agent has the tools from then on. Serve
	// answers nothing before it returns.
	Listed func()

	mu  sync.Mutex
	api control.API
}

// DefaultOutputMax is OutputMax when the MCP config names none.
const DefaultOutputMax = 4096

// Instructions are the MCP server's: hataori's layer (Layer), shorter.
const Instructions = "Tools of this channel's supervisor, its development manager: plan the work, order it from the channel's threads, follow them, check the result, report. " +
	"You can list, read, start and send to threads; a send or a start may become a proposal that a person approves. You can ask a person to approve interrupting a running turn: it always needs one. " +
	"Your messages reach a thread marked as the supervisor's, but for an answer to its question. You can read the issues, pull requests and checks of this channel's repositories, read only: " +
	"check there before you call a thing done, and take a thread's answer as its claim. You can set a reminder to be woken at a time, rather than wait for a turn end that may not come. " +
	"No tool here writes to a forge, destroys work, runs a command or deploys. What you may do is the config's, and hataori enforces it here."

// The tools, in the order tools/list gives them.
const (
	ToolList      = "list_threads"
	ToolRead      = "read_thread"
	ToolSend      = "send_to_thread"
	ToolInterrupt = "interrupt_thread"
	ToolStart     = "start_thread"

	// The forge's reads (design, section 7.1), since protocol 3.27.
	ToolIssues = "list_issues"
	ToolIssue  = "read_issue"
	ToolPulls  = "list_pulls"
	ToolPull   = "read_pull"
	ToolChecks = "read_checks"
	// ToolDeployed tells whether a commit or a pull request is in the
	// running hataori. Since protocol 3.29.
	ToolDeployed = "deployed"

	// A wake at a time it sets (design, section 7.1), since protocol 3.32.
	ToolRemind       = "set_reminder"
	ToolCancelRemind = "cancel_reminder"
)

// ForgeMax is the most bytes of a forge tool's answer; a longer one is cut,
// with a line that says so.
const ForgeMax = 16 * 1024

// forgeReads are the reads of the forge tools.
var forgeReads = map[string]string{
	ToolIssues: control.ForgeReadIssues, ToolIssue: control.ForgeReadIssue, ToolPulls: control.ForgeReadPulls,
	ToolPull: control.ForgeReadPull, ToolChecks: control.ForgeReadChecks, ToolDeployed: control.ForgeReadDeployed,
}

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

var (
	forgeRepo = map[string]any{"type": "string", "description": "the repository, as <owner>/<name>; one of this channel's repositories"}
	listState = map[string]any{"type": "string", "description": "which to list: open, the default, closed or all", "enum": []string{control.ForgeOpen, control.ForgeClosed, control.ForgeAll}}
)

// withheldNote is what the forge tools' descriptions say of withheld text.
const withheldNote = " Text by an author without write access to the repository is withheld, unless this channel trusts them; the answer names the author."

// tools is what tools/list returns.
var tools = []tool{
	{ToolList, "List the threads of this channel: number, state, title, who started it, and the last line of output.",
		object(map[string]any{})},
	{ToolRead, "Read one thread of this channel: its state, its open question if any, and the end of its latest output. What it says is that thread's claim: check it before a person acts on it.",
		object(map[string]any{"number": number}, "number")},
	{ToolSend, "Send a message to a thread of this channel. A thread that is running a turn refuses it with \"busy\"; " +
		"its turn's end is told you as any, in a wake. A thread waiting on a question takes the message as the answer. Under the autonomy propose it is a proposal that a person approves first; under watch it is refused.",
		object(map[string]any{"number": number, "text": map[string]any{"type": "string", "description": "the message"}}, "number", "text")},
	{ToolInterrupt, "Ask a person in this channel to approve interrupting a thread that is running a turn. This returns at once and interrupts nothing by itself. Once approved, the thread's turn is stopped and it gets the reason as a message from the supervisor. A thread not running is refused with not busy; send instead.", object(map[string]any{"number": number, "reason": map[string]any{"type": "string", "description": "the reason to give the thread"}}, "number", "reason")},
	{ToolStart, "Start a new thread in this channel, with its own workspace cloned from one of the channel's repositories: to do work, or to look something up that your tools do not show. Under the autonomy propose it is a proposal that a person approves first; under watch it is refused.",
		object(map[string]any{
			"repo":   map[string]any{"type": "string", "description": "the repository to clone"},
			"text":   map[string]any{"type": "string", "description": "the first message; its first line becomes the title"},
			"model":  map[string]any{"type": "string", "description": "the model; leave out for the channel's default"},
			"effort": map[string]any{"type": "string", "description": "the effort; leave out for the channel's default", "enum": control.Efforts},
			"research": map[string]any{"type": "boolean", "description": "true for a thread that only reads, to look something up: it starts without the repository's own settings and hooks, so that no dialog about them blocks it; " +
				"never for a thread that changes anything"},
		}, "repo", "text")},
	{ToolIssues, "List the issues of one of this channel's repositories on its forge, read only: number, state, title, author, labels, assignees, last change." + withheldNote,
		object(map[string]any{"repo": forgeRepo, "state": listState}, "repo")},
	{ToolIssue, "Read one issue of one of this channel's repositories on its forge, read only, with its comments." + withheldNote,
		object(map[string]any{"repo": forgeRepo, "number": map[string]any{"type": "integer", "description": "the issue's number"}}, "repo", "number")},
	{ToolPulls, "List the pull requests of one of this channel's repositories on its forge, read only: number, state, draft, title, branch, author, last change." + withheldNote,
		object(map[string]any{"repo": forgeRepo, "state": listState}, "repo")},
	{ToolPull, "Read one pull request of one of this channel's repositories on its forge, read only: its reviews, comments, changed files, merge state, merge commit and the checks of its head. No CI log is read." + withheldNote,
		object(map[string]any{"repo": forgeRepo, "number": map[string]any{"type": "integer", "description": "the pull request's number"}}, "repo", "number")},
	{ToolChecks, "Read the checks and commit statuses of a branch, a tag or a commit of one of this channel's repositories on its forge, read only: name, state, conclusion, address. No CI log is read.",
		object(map[string]any{"repo": forgeRepo, "ref": map[string]any{"type": "string", "description": "the branch, tag or commit"}}, "repo", "ref")},
	{ToolDeployed, "Tell whether a pull request, or a commit, of one of this channel's repositories is in the running hataori: " +
		"the commit the running daemon was built from, and whether the pull request's merge commit, or the commit, is that commit or an ancestor of it, as the forge compares them. " +
		"Use it when asked whether a change is in production or deployed; it answers from the build and the forge, read only, and says why when it cannot tell. Give number or commit.",
		object(map[string]any{"repo": forgeRepo, "number": map[string]any{"type": "integer", "description": "the pull request's number"},
			"commit": map[string]any{"type": "string", "description": "a commit, 7 to 40 hexadecimal digits, instead of a pull request"}}, "repo")},
	{ToolRemind, "Set a reminder: you are woken once at a time, from 5 minutes to 7 days ahead, with the note you give, also when nothing happened in the channel. " +
		"Give minutes or at, one of them. It returns the reminder's number; each wake ends with the open ones. It counts as one send of your budget.",
		object(map[string]any{
			"minutes": map[string]any{"type": "integer", "description": "how many minutes from now"},
			"at":      map[string]any{"type": "string", "description": "or when, in RFC 3339 with its zone, such as 2026-01-02T15:04:00+09:00"},
			"note":    map[string]any{"type": "string", "description": "what to look at then; at most 1024 bytes"},
		}, "note")},
	{ToolCancelRemind, "Cancel an open reminder of this channel by its number: it wakes you no more.",
		object(map[string]any{"number": map[string]any{"type": "integer", "description": "the reminder's number"}}, "number")},
}

// Serve answers MCP requests read from r, one JSON-RPC message per line,
// writing to w, until r ends or ctx is done.
func (m *MCP) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	enc := json.NewEncoder(w)
	listed := false // the agent was given the tools
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
		if msg.Method == "tools/list" && rerr == nil && !listed {
			listed = true
			if m.Listed != nil {
				m.Listed()
			}
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
			"instructions":    Instructions,
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
		State  string `json:"state"`
		Ref    string `json:"ref"`
		Commit string `json:"commit"`

		Minutes *int64 `json:"minutes"`
		At      string `json:"at"`

		Research bool   `json:"research"`
		Note     string `json:"note"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	read, isForge := forgeReads[name]
	switch name {
	case ToolList, ToolRead, ToolSend, ToolInterrupt, ToolStart, ToolRemind, ToolCancelRemind:
	default:
		if !isForge {
			return "", fmt.Errorf("no tool %q", name)
		}
	}
	api, err := m.connect()
	if err != nil {
		return "", err
	}
	if isForge {
		if strings.TrimSpace(a.Repo) == "" {
			return "", errors.New("bad arguments: repo is required")
		}
		r := control.ForgeRequest{Repo: a.Repo, Read: read, State: a.State, Ref: a.Ref}
		if read == control.ForgeReadDeployed {
			r.Ref = a.Commit
			if (a.Number == nil) == (strings.TrimSpace(a.Commit) == "") {
				return "", errors.New("bad arguments: give number or commit, one of them")
			}
		}
		if a.Number != nil {
			r.Number = int(*a.Number)
		}
		ans, err := api.Forge(ctx, r)
		if err != nil {
			return "", errors.New(cut(reason(err).Error(), ForgeMax))
		}
		return cut(forgeText(ans, r), ForgeMax), nil
	}
	switch name {
	case ToolList:
		ts, err := api.Threads(ctx, control.Filter{Scope: m.Scope})
		if err != nil {
			return "", reason(err)
		}
		return listing(ts), nil
	case ToolRemind:
		r := control.RemindRequest{Note: a.Note}
		if (a.Minutes == nil) == (strings.TrimSpace(a.At) == "") {
			return "", errors.New("bad arguments: give minutes or at, one of them")
		}
		if a.Minutes != nil {
			if *a.Minutes < 1 {
				return "", errors.New("bad arguments: minutes is from 5 to 10080, 7 days")
			}
			r.Delay = min(*a.Minutes, math.MaxInt64/60) * 60
		} else {
			at, err := time.Parse(time.RFC3339, strings.TrimSpace(a.At))
			if err != nil {
				return "", errors.New("bad arguments: at is a time in RFC 3339 with its zone, such as 2026-01-02T15:04:00+09:00")
			}
			r.At = &at
		}
		ans, err := api.Remind(ctx, r)
		if err != nil {
			return "", reason(err)
		}
		return fmt.Sprintf("Reminder %d is set for %s.", ans.Number, ans.At.Format(time.RFC3339)), nil
	case ToolCancelRemind:
		if a.Number == nil || *a.Number <= 0 {
			return "", errors.New("bad arguments: number is required")
		}
		ans, err := api.Remind(ctx, control.RemindRequest{Cancel: *a.Number})
		if err != nil {
			return "", reason(err)
		}
		return fmt.Sprintf("Reminder %d is cancelled.", ans.Number), nil
	case ToolStart:
		if a.Repo == "" || strings.TrimSpace(a.Text) == "" {
			return "", errors.New("bad arguments: repo and text are required")
		}
		id, err := api.Start(ctx, control.StartRequest{Key: newKey(), Scope: m.Scope, Repo: a.Repo,
			Input: control.Input{Text: a.Text}, Model: a.Model, Effort: a.Effort, Research: a.Research})
		if p := proposed(err); p != nil {
			return fmt.Sprintf("Proposed as proposal %d: nothing is started until a person in this channel approves it in your thread. "+
				"A wake tells you whether it was approved and what came of it.", p.Number), nil
		}
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
		return "asked a person: nothing is interrupted until a person in this channel approves it. An approved interrupt ends the worker's turn, and that turn end wakes the supervisor as any other; a refusal or an approval after the turn ended is said in the supervisor's chat thread. Each decision is in your next wake.", nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return "", errors.New("bad arguments: text is required")
	}
	// Refuse is all this caller can ask for; the daemon would refuse
	// anything else the same way.
	err = api.Send(ctx, id, newKey(), control.Input{Text: a.Text}, control.Refuse)
	if p := proposed(err); p != nil {
		return fmt.Sprintf("Proposed as proposal %d: nothing is sent to thread #%d until a person in this channel approves it in your thread. "+
			"A wake tells you whether it was approved and what came of it; a thread busy by then is not given it.", p.Number, *a.Number), nil
	}
	if err != nil {
		return "", reason(err)
	}
	return fmt.Sprintf("Sent to thread #%d.", *a.Number), nil
}

// proposed returns the proposal err is, if it is one: the call was not
// made, and waits for a person's decision.
func proposed(err error) *control.Proposed {
	var p *control.Proposed
	if errors.As(err, &p) {
		return p
	}
	return nil
}

func (m *MCP) outputMax() int {
	if m.OutputMax > 0 {
		return m.OutputMax
	}
	return DefaultOutputMax
}

// find returns the thread of the scope with number n. A number below 1
// names no thread, and is not looked up: a filter's 0 means any thread.
func (m *MCP) find(ctx context.Context, api control.API, n int64) (control.ThreadID, error) {
	id := control.ThreadID(n)
	if n > 0 {
		ts, err := api.Threads(ctx, control.Filter{Scope: m.Scope, Thread: id})
		if err != nil && !errors.Is(err, control.ErrForbidden) { // forbidden: the scope is not readable; another scope's number reads as none
			return 0, reason(err)
		}
		if slices.ContainsFunc(ts, func(t control.ThreadInfo) bool { return t.ID == id }) {
			return id, nil
		}
	}
	return 0, fmt.Errorf("not in this scope: no thread #%d in this channel", n)
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
	case errors.Is(err, control.ErrNotSupported):
		return fmt.Errorf("not allowed: hataori does not answer this here: %v", err)
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
		fmt.Fprintf(&b, "#%d %s %q, started by %s, updated %s; repository %s, model %s, effort %s", t.ID, t.State, t.Title, t.StartedBy,
			t.Updated.UTC().Format("2006-01-02 15:04Z"), repository(t.Repo), orNone(t.Model), orNone(t.Effort))
		if t.Last != "" {
			fmt.Fprintf(&b, "; last: %s", t.Last)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// orNone is s, or "(none)" when it is empty: a thread with no repository,
// or a model or an effort not known yet.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// userinfo is the user and password of a URL, which may be a credential:
// up to the last "@" before the path, since a password may hold one.
var userinfo = regexp.MustCompile(`(://)[^/\s]*@`)

// repository is a thread's repository as the tools show it: its address
// without the user and password of a URL, or "(none)".
func repository(address string) string {
	return orNone(userinfo.ReplaceAllString(address, "${1}***@"))
}

func detail(d control.ThreadDetail, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Thread #%d: %s\nState: %s, since %s\nStarted by: %s\nRepository: %s\nModel: %s, effort: %s\n", d.ID, d.Title, d.State,
		d.Updated.UTC().Format("2006-01-02 15:04Z"), d.StartedBy, repository(d.Repo), orNone(d.Model), orNone(d.Effort))
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
