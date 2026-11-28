package host

import (
	"bufio"
	"context"
	"encoding/json"
	"time"

	"github.com/yousan/hataori/internal/nativehttp"
)

// ComputedSocketPath is the host's endpoint derived solely from its state
// directory. Desktop compatibility checks must not follow SocketPath's saved
// long-path pointer. It is not evidence that a process owns a host.
func ComputedSocketPath(stateDir string) string { return defaultSocket(stateDir) }

// ProbeInputCapability asks the selected directory's private, kernel-UID-checked
// endpoint on one bounded connection. No saved PID/pointer supplies authority,
// and the operation cannot stop a command or modify its input.
func ProbeInputCapability(ctx context.Context, stateDir string) (InputCapability, error) {
	conn, err := nativehttp.DialOwnerSocket(ctx, defaultSocket(stateDir))
	if err != nil {
		return InputCapability{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	if by, ok := ctx.Deadline(); ok && by.Before(deadline) {
		deadline = by
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return InputCapability{}, err
	}
	c := &Client{conn: conn, dec: json.NewDecoder(bufio.NewReader(conn))}
	return c.InputCapability()
}
