// Package wire carries the control API over a Unix socket: a server in the
// daemon and a client for other processes, speaking one JSON object per
// line.
//
// A connection starts with a hello from each side carrying the protocol
// version. Then the client sends requests and the server answers each one
// in order. An Events request turns the connection into a stream of events
// until the client closes it.
package wire

import (
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

// Version is the protocol version this build speaks.
const Version = "1.0"

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
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The arguments of each operation.
type (
	threadsArgs struct{ Filter control.Filter }
	threadArgs  struct {
		ID     control.ThreadID
		Recent int
	}
	sendArgs struct {
		ID    control.ThreadID
		Key   string
		Input control.Input
		Busy  control.Busy
	}
	answerArgs struct {
		Request control.RequestID
		Answer  control.Answer
	}
	stopArgs   struct{ ID control.ThreadID }
	eventsArgs struct {
		Filter control.Filter
		After  uint64
	}
)

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
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	short := filepath.Join(base, "hataori-"+hex.EncodeToString(sum[:6]), "control.sock")
	if len(short) > MaxSocketPath {
		return "", fmt.Errorf("wire: no socket path short enough for root %s", root)
	}
	return short, nil
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
