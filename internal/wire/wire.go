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
const Version = "1.10"

type hello struct {
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
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
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
	sendArgs struct {
		ID    control.ThreadID `json:"id"`
		Key   string           `json:"key"`
		Input control.Input    `json:"input"`
		Busy  control.Busy     `json:"busy"`
	}
	answerArgs struct {
		Request control.RequestID `json:"request"`
		Answer  control.Answer    `json:"answer"`
	}
	stopArgs struct {
		ID control.ThreadID `json:"id"`
	}
	workspaceArgs struct {
		ID control.ThreadID `json:"id"`
	}
	eventsArgs struct {
		Filter control.Filter `json:"filter"`
		After  uint64         `json:"after"`
	}
	signInLinkArgs struct{}
	terminalArgs   struct {
		ID    control.ThreadID `json:"id"`
		Run   string           `json:"run"`
		After uint64           `json:"after"`
	}
	hostTerminalsArgs struct{}
	hostScreenArgs    struct {
		Pane string `json:"pane"`
	}
)

// operation is one operation of the control API on the wire: the type of
// its arguments and of its result, and how the server runs it. Events and
// Terminal have no run: each turns the connection into a stream.
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
	"Start": {control.StartRequest{}, control.ThreadID{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a control.StartRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.Start(ctx, a)
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
	"Stop": {stopArgs{}, nil, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a stopArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, api.Stop(ctx, a.ID)
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
		if err := args(&signInLinkArgs{}); err != nil {
			return nil, err
		}
		return api.SignInLink(ctx)
	}},
	"Terminal": {terminalArgs{}, control.TerminalChunk{}, nil},
	"HostTerminals": {hostTerminalsArgs{}, []control.HostTerminal{}, func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		return api.HostTerminals(ctx)
	}},
	"HostScreen": {hostScreenArgs{}, "", func(ctx context.Context, api control.API, args func(any) error) (any, error) {
		var a hostScreenArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return api.HostScreen(ctx, a.Pane)
	}},
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
	{"not_supported", ErrNotSupported},
}

func encodeError(err error) *wireError {
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
