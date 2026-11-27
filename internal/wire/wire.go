// Package wire carries the control API over a Unix socket: a server in the
// daemon and a client for other processes, speaking one JSON object per
// line.
//
// A connection starts with a hello from each side carrying the protocol
// version. Then either side may send requests, and the other answers each
// one. An Events or a Terminal request from the client turns the
// connection into a stream until the client closes it. docs/protocol.md is the
// description of the protocol.
package wire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/session"
)

// Version is the protocol version this build speaks: major.minor. Two
// sides work together when their major numbers are equal (docs/protocol.md).
const Version = "3.45"

type hello struct {
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
	// Token is a credential the daemon gave a client, which binds the
	// connection to a limited caller; only a client sends it. Since 2.2.
	Token string `json:"token,omitempty"`
}

type request struct {
	ID   uint64          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

type response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
	Event  *control.Event  `json:"event,omitempty"`
	// Terminal is one chunk of a Terminal stream.
	Terminal *control.TerminalChunk `json:"terminal,omitempty"`
	// Ended is the reason an OperateHost stream's operation ended, in the
	// last message of the stream. Since 3.11.
	Ended string `json:"ended,omitempty"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Proposal is the proposal's number, for the code proposed. Since 3.26.
	Proposal int64 `json:"proposal,omitempty"`
}

// The arguments of each operation.
type (
	threadsArgs struct {
		Filter control.Filter `json:"filter"`
	}
	threadArgs struct {
		ID     control.ThreadID `json:"id"`
		Recent int              `json:"recent"`
		Events int              `json:"events,omitempty"` // since 1.5
	}
	threadEventsArgs struct { // since 3.3
		ID     control.ThreadID `json:"id"`
		Before uint64           `json:"before,omitempty"`
		Limit  int              `json:"limit"`
	}
	scopesArgs  struct{} // since 3.5
	setNameArgs struct { // since 3.5
		Link control.LinkRef `json:"link"`
		Name string          `json:"name,omitempty"`
	}
	layoutArgs    struct{} // since 3.21
	setLayoutArgs struct { // since 3.21
		Client string                `json:"client"`
		Groups []control.LayoutGroup `json:"groups"`
	}
	sendArgs struct {
		ID    control.ThreadID `json:"id"`
		Key   string           `json:"key"`
		Input control.Input    `json:"input"`
		Busy  control.Busy     `json:"busy"`
	}
	askInterruptArgs struct {
		ID     control.ThreadID `json:"id"`
		Key    string           `json:"key"`
		Reason string           `json:"reason"`
	}
	answerArgs struct {
		Request control.RequestID `json:"request"`
		Answer  control.Answer    `json:"answer"`
	}
	stopArgs struct {
		ID   control.ThreadID `json:"id"`
		Turn int64            `json:"turn,omitempty"` // since 3.14
	}
	linkArgs struct { // since 3.4
		ID   control.ThreadID `json:"id"`
		Link control.LinkRef  `json:"link"`
	}
	closeArgs struct { // since 2.3
		ID control.ThreadID `json:"id"`
	}
	removeArgs struct { // since 2.3
		ID control.ThreadID `json:"id"`
	}
	reopenArgs struct { // since 3.31
		ID control.ThreadID `json:"id"`
	}
	releaseArgs struct { // since 3.17
		ID control.ThreadID `json:"id"`
	}
	trustArgs struct { // since 3.28
		ID      control.ThreadID `json:"id"`
		Session int64            `json:"session"`
		Files   []string         `json:"files"`
	}
	startWithoutSettingsArgs struct { // since 3.28
		ID      control.ThreadID `json:"id"`
		Session int64            `json:"session"`
	}
	trustsArgs  struct{} // since 3.28
	untrustArgs struct { // since 3.28
		ID int64 `json:"id"`
	}
	workspaceArgs struct {
		ID control.ThreadID `json:"id"`
	}
	eventsArgs struct {
		Filter control.Filter `json:"filter"`
		After  uint64         `json:"after"`
	}
	signInLinkArgs struct {
		Way control.Way `json:"way,omitempty"` // since 3.2
	}
	terminalArgs struct {
		ID    control.ThreadID `json:"id"`
		Run   string           `json:"run"`
		After uint64           `json:"after"`
	}
	attachTerminalArgs struct {
		ID control.ThreadID `json:"id"`
	}
	hostTerminalsArgs struct{}
	limitsArgs        struct{}
	clientsArgs       struct{}
	hostScreenArgs    struct {
		Pane string `json:"pane"`
	}
	operateHostArgs struct { // since 3.11
		Pane string `json:"pane"`
	}
	hostKeysArgs struct { // since 3.11, on an OperateHost stream only
		Keys []control.HostKey `json:"keys"`
	}
	endArgs struct{} // since 3.11, on an OperateHost stream only
)

// hostOperation is OperateHost's result. Since 3.11.
type hostOperation struct {
	Idle int64 `json:"idle"` // seconds without a key after which the operation ends
}

// operation is one operation of the control API on the wire: the type of
// its arguments and of its result, and how the server runs it. Events,
// Terminal and OperateHost have no run: each turns the connection into a
// stream. HostKeys and End have none either: they are sent only on an
// OperateHost stream.
type operation struct {
	args, result any
	run          func(ctx context.Context, api control.API, args func(any) error) (any, error)
}

// operations is every operation a client may send. docs/protocol.md
// describes each, and a test keeps the two equal.
var operations = map[string]operation{
	"Threads": {threadsArgs{}, []control.ThreadInfo{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a threadsArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Threads(ctx, a.Filter)
	}},
	"Thread": {threadArgs{}, control.ThreadDetail{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a threadArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Thread(ctx, a.ID, a.Recent, a.Events)
	}},
	"ThreadEvents": {threadEventsArgs{}, []control.Event{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a threadEventsArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.ThreadEvents(ctx, a.ID, a.Before, a.Limit)
	}},
	"Scopes": {scopesArgs{}, []control.ScopeInfo{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.Scopes(ctx)
	}},
	"SetName": {setNameArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a setNameArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.SetName(ctx, a.Link, a.Name)
	}},
	"Layout": {layoutArgs{}, []control.ChatLayout{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.Layout(ctx)
	}},
	"SetLayout": {setLayoutArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a setLayoutArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.SetLayout(ctx, a.Client, a.Groups)
	}},
	"Start": {control.StartRequest{}, control.ThreadID(0), func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a control.StartRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Start(ctx, a)
	}},
	"TakeIn": {control.TakeInRequest{}, control.ThreadID(0), func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a control.TakeInRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.TakeIn(ctx, a)
	}},
	"Release": {releaseArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a releaseArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Release(ctx, a.ID)
	}},
	"Trust": {trustArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a trustArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Trust(ctx, a.ID, a.Session, a.Files)
	}},
	"StartWithoutSettings": {startWithoutSettingsArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a startWithoutSettingsArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.StartWithoutSettings(ctx, a.ID, a.Session)
	}},
	"Trusts": {trustsArgs{}, []control.TrustedFile{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.Trusts(ctx)
	}},
	"Untrust": {untrustArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a untrustArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Untrust(ctx, a.ID)
	}},
	"Send": {sendArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a sendArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Send(ctx, a.ID, a.Key, a.Input, a.Busy)
	}},
	"Answer": {answerArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a answerArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Answer(ctx, a.Request, a.Answer)
	}},
	"AskInterrupt": {askInterruptArgs{}, control.RequestID(""), func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a askInterruptArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.AskInterrupt(ctx, a.ID, a.Key, a.Reason)
	}},
	"Stop": {stopArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a stopArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Stop(ctx, a.ID, a.Turn)
	}},
	"Link": {linkArgs{}, control.LinkRef{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a linkArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Link(ctx, a.ID, a.Link)
	}},
	"Close": {closeArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a closeArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.CloseThread(ctx, a.ID)
	}},
	"Reopen": {reopenArgs{}, control.Reopened{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a reopenArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.ReopenThread(ctx, a.ID)
	}},
	"Remove": {removeArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a removeArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.RemoveThread(ctx, a.ID)
	}},
	"Workspace": {workspaceArgs{}, control.WorkspaceState{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a workspaceArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Workspace(ctx, a.ID)
	}},
	"Events": {eventsArgs{}, control.Event{}, nil},
	"SignInLink": {signInLinkArgs{}, "", func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a signInLinkArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.SignInLink(ctx, a.Way)
	}},
	"Terminal": {terminalArgs{}, control.TerminalChunk{}, nil},
	"HostTerminals": {hostTerminalsArgs{}, []control.HostTerminal{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.HostTerminals(ctx)
	}},
	"AttachTerminal": {attachTerminalArgs{}, control.Attachment{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a attachTerminalArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.AttachTerminal(ctx, a.ID)
	}},
	"AttachWorker": {attachTerminalArgs{}, control.WorkerAttachment{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a attachTerminalArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		worker, ok := api.(control.WorkerAttacher)
		if !ok {
			return nil, fmt.Errorf("wire: worker attachment is unavailable")
		}
		return worker.AttachWorker(ctx, a.ID)
	}},
	"Limits": {limitsArgs{}, []control.AgentLimits{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.Limits(ctx)
	}},
	"Clients": {clientsArgs{}, []control.ClientState{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.Clients(ctx)
	}},
	"Forge": {control.ForgeRequest{}, control.ForgeAnswer{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a control.ForgeRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Forge(ctx, a)
	}},
	"Remind": {control.RemindRequest{}, control.RemindAnswer{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a control.RemindRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Remind(ctx, a)
	}},
	"Files": {control.FilesRequest{}, control.FilesAnswer{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a control.FilesRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Files(ctx, a)
	}},
	"HostScreen": {hostScreenArgs{}, "", func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a hostScreenArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.HostScreen(ctx, a.Pane)
	}},
	"HostView": {hostScreenArgs{}, control.HostView{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a hostScreenArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.HostView(ctx, a.Pane)
	}},
	"OperateHost": {operateHostArgs{}, hostOperation{}, nil},
	"HostKeys":    {hostKeysArgs{}, nil, nil},
	"End":         {endArgs{}, nil, nil},
}

// The error codes on the wire, for the errors a client can act on.
var codes = []struct {
	code string
	err  error
}{
	{"forbidden", core.ErrForbidden},
	{"disabled", core.ErrDisabled},
	{"busy", core.ErrBusy},
	{"not_found", core.ErrNotFound},
	{"refused", session.ErrRefused},
	// After refused: an error that wraps both, as AttachTerminal's to a
	// closed thread, stays refused, as 2.1 and 2.2 answered it.
	{"closed", core.ErrClosed},                       // since 2.3
	{"removal_refused", core.ErrRemovalRefused},      // since 2.3
	{"linked_elsewhere", control.ErrLinkedElsewhere}, // since 3.4
	{"ended", control.ErrEnded},                      // since 3.11
	{"stopped", control.ErrStopped},                  // since 3.18
	{"proposed", control.ErrProposed},                // since 3.26
	{"reopen_refused", control.ErrReopenRefused},     // since 3.31
	{"not_supported", ErrNotSupported},
}

func encodeError(err error) *wireError {
	var p *control.Proposed
	if errors.As(err, &p) {
		return &wireError{Code: "proposed", Message: err.Error(), Proposal: p.Number}
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return &wireError{Code: c.code, Message: err.Error()}
		}
	}
	return &wireError{Code: "error", Message: err.Error()}
}

// remoteError is an error the daemon returned. It matches the sentinel of
// its code with errors.Is.
type remoteError struct {
	code, msg string
}

func (e *remoteError) Error() string { return e.msg }

func (e *remoteError) Is(target error) bool {
	for _, c := range codes {
		if c.code == e.code && c.err == target {
			return true
		}
	}
	return false
}

func decodeError(e *wireError) error {
	if e == nil {
		return nil
	}
	if e.Code == "proposed" {
		return &control.Proposed{Number: e.Proposal}
	}
	return &remoteError{code: e.Code, msg: e.Message}
}

func versionError(client, daemon string) error {
	return fmt.Errorf("wire: protocol version mismatch: client speaks %s, daemon speaks %s", client, daemon)
}

// MaxSocketPath is the longest socket path used. The platforms allow 104
// (macOS) or 108 (Linux) bytes including the terminating zero.
const MaxSocketPath = 100

// Paths relative to the instance root.
const (
	SocketFile  = "state/control.sock"
	pointerFile = "state/control.sock.path" // holds the socket path when it is not SocketFile
)

// SocketPath returns where the daemon of root listens. When
// <root>/state/control.sock is too long, it is a short path in the user's
// runtime directory, recorded in <root>/state/control.sock.path for clients.
func SocketPath(root string) (string, error) {
	p := filepath.Join(root, SocketFile)
	if len(p) <= MaxSocketPath {
		return p, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	name := filepath.Join("hataori-"+hex.EncodeToString(sum[:6]), "control.sock")
	for _, base := range []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir(), "/tmp"} {
		if base == "" {
			continue
		}
		if short := filepath.Join(base, name); len(short) <= MaxSocketPath {
			return short, nil
		}
	}
	return "", fmt.Errorf("wire: no socket path short enough for root %s", root)
}

// clientPath returns where a client finds the daemon of root.
func clientPath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, pointerFile))
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Join(root, SocketFile), nil
}
