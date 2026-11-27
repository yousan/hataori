package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Client talks to the host of one state directory. It is safe for
// concurrent use; its calls are sent one at a time, in order.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	dec  *json.Decoder
}

// Dial connects to the host in stateDir. When the command has already
// exited, the error wraps os.ErrNotExist or syscall.ECONNREFUSED.
func Dial(stateDir string) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return DialContext(ctx, stateDir)
}

// DialContext is Dial, giving up when ctx ends.
func DialContext(ctx context.Context, stateDir string) (*Client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", SocketPath(stateDir))
	if err != nil {
		return nil, fmt.Errorf("host: connect: %w", err)
	}
	return &Client{conn: c, dec: json.NewDecoder(bufio.NewReader(c))}, nil
}

// DialIdentity connects to the socket computed for this owned state directory,
// never to a stored socket-pointer address, and verifies the live host's opaque
// identity. It is for callers that reserved the identity before launching.
func DialIdentity(ctx context.Context, stateDir, identity string) (*Client, error) {
	if identity == "" {
		return nil, errors.New("host: empty owned-run identity")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", defaultSocket(stateDir))
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, dec: json.NewDecoder(bufio.NewReader(conn))}
	got, err := c.Identity()
	if err != nil || got != identity {
		c.Close()
		return nil, errors.New("host: owned-run identity does not match")
	}
	return c, nil
}

// Close closes the connection; the host keeps running.
func (c *Client) Close() error { return c.conn.Close() }

// Identity returns the caller-reserved identity held by the live host. An old
// host that lacks the operation refuses; an empty identity never proves that
// an externally recorded run owns this host.
func (c *Client) Identity() (string, error) {
	c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	defer c.conn.SetDeadline(time.Time{})
	r, err := c.call(request{Op: "identity"})
	return r.Identity, err
}

// Deadline bounds operations on a dedicated connection. Callers sharing a
// Client must coordinate deadline changes with their own calls.
func (c *Client) Deadline(t time.Time) error { return c.conn.SetDeadline(t) }

// End stops this connected host's command, keeping a verified connection
// rather than dialing a pathname again after checking its run identity. The
// caller reads the recorded final status afterwards; no PID is taken from it
// as permission to signal a process.
func (c *Client) End(interrupt, terminate time.Duration) (StopResult, error) {
	r, err := c.call(request{Op: "stop", Interrupt: interrupt, Terminate: terminate})
	return StopResult{Already: r.Already, Signals: r.Signals}, err
}

func (c *Client) call(req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := json.NewEncoder(c.conn).Encode(req); err != nil {
		return response{}, fmt.Errorf("host: %s: %w", req.Op, err)
	}
	var resp response
	if err := c.dec.Decode(&resp); err != nil {
		return response{}, fmt.Errorf("host: %s: %w", req.Op, err)
	}
	if resp.Error != "" {
		return resp, errors.New("host: " + resp.Error)
	}
	return resp, nil
}

// Input writes b to the command's terminal or standard input, unchanged.
// It returns once the host has written all of it.
func (c *Client) Input(b []byte) error {
	_, err := c.call(request{Op: "input", Data: b})
	return err
}

// Tail returns the last n bytes of the command's raw log (its terminal, or
// its standard error on pipes), for a person to read.
func (c *Client) Tail(n int) ([]byte, error) {
	resp, err := c.call(request{Op: "tail", Interrupt: time.Duration(n)})
	return resp.Data, err
}

// PID returns the command's process id; 0 for a relay, which runs none.
func (c *Client) PID() (int, error) {
	resp, err := c.call(request{Op: "status"})
	return resp.PID, err
}

// Device returns the name of the terminal the command runs in, such as
// /dev/pts/3 or /dev/ttys003; "" when it runs on pipes, and for a relay,
// whose terminal another holder holds.
// It gives up at ctx's deadline, or after a second when ctx has none.
func (c *Client) Device(ctx context.Context) (string, error) {
	by, ok := ctx.Deadline()
	if !ok {
		by = time.Now().Add(time.Second)
	}
	c.conn.SetDeadline(by)
	defer c.conn.SetDeadline(time.Time{})
	resp, err := c.call(request{Op: "status"})
	return resp.Device, err
}

// Attached returns how many terminals of people are attached to the host
// (Attach), and when the last one went, the zero time for never. It gives
// up at ctx's deadline, or after a second when ctx has none.
func (c *Client) Attached(ctx context.Context) (n int, detached time.Time, err error) {
	by, ok := ctx.Deadline()
	if !ok {
		by = time.Now().Add(time.Second)
	}
	c.conn.SetDeadline(by)
	defer c.conn.SetDeadline(time.Time{})
	resp, err := c.call(request{Op: "status"})
	if err != nil {
		return 0, time.Time{}, err
	}
	if resp.Detached > 0 {
		detached = time.UnixMilli(resp.Detached)
	}
	return resp.Attached, detached, nil
}

// Gone reports whether the host of stateDir has finished: its socket is
// gone and its status recorded.
func Gone(stateDir string) bool {
	_, ok, _ := ReadStatus(stateDir)
	if !ok {
		return false
	}
	_, err := os.Stat(SocketPath(stateDir))
	return errors.Is(err, os.ErrNotExist)
}

// screenTimeout bounds one Screen call.
const screenTimeout = 2 * time.Second

// Screen returns what the command of the host's current run wrote to its
// terminal after position from, counted from the run's first byte, and the
// position to ask from next time. It is for a codec watching an agent's
// start only. The host is asked, as it alone knows where its run's bytes
// are in the log it keeps: the log also holds earlier runs' output, and a
// trim moves what it keeps (#520). When the host cannot be asked (on
// pipes, or once it has gone), there is nothing.
//
// A host started by an earlier hataori, which a daemon that died during a
// launch can leave running, does not know the request. Its log is read as
// it was before, from the file's byte from on: what its run wrote, if it
// is the log's first run and nothing was trimmed.
func Screen(stateDir string, from uint64) ([]byte, uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), screenTimeout)
	defer cancel()
	c, err := DialContext(ctx, stateDir)
	if err != nil {
		return nil, from
	}
	defer c.Close()
	c.conn.SetDeadline(time.Now().Add(screenTimeout))
	resp, err := c.call(request{Op: "screen", After: from})
	if err != nil {
		if strings.HasPrefix(resp.Error, "unknown operation") {
			return keptSince(stateDir, from)
		}
		return nil, from
	}
	return resp.Data, resp.Pos
}

// keptSince returns the terminal log kept in stateDir after its first from
// bytes, and its length.
func keptSince(stateDir string, from uint64) ([]byte, uint64) {
	b, err := os.ReadFile(filepath.Join(stateDir, TerminalLog))
	if err != nil || uint64(len(b)) <= from {
		return nil, from
	}
	return b[from:], uint64(len(b))
}

// StopResult says what Stop did.
type StopResult struct {
	Already bool   // the command had already exited; nothing was sent
	Signals string // the signals sent, in order, such as "SIGINT,SIGTERM"
	Status  Status // how the command ended, as recorded
	Left    string // why processes were left running that may be the command's; "" when none were
}

// Stop ends the command of the host in stateDir and leaves no process of
// its group: SIGINT, then SIGTERM after interrupt, then SIGKILL after
// terminate. It returns once the host has recorded the command's status,
// so its logs are complete. A command that already exited is not an error.
// A relay (Spec.Relay) is sent no signal: it ends, and its Status says
// Relay.
func Stop(stateDir string, interrupt, terminate time.Duration) (StopResult, error) {
	var r StopResult
	c, err := dialOrEnd(stateDir)
	if err == nil {
		resp, cerr := c.call(request{Op: "stop", Interrupt: interrupt, Terminate: terminate})
		c.Close()
		r.Already, r.Signals = resp.Already, resp.Signals
		if cerr != nil && !isGoneError(cerr) {
			return r, cerr
		}
	} else if _, ok, _ := ReadStatus(stateDir); !ok {
		return r, err // no host and no status: nothing to say
	} else {
		r.Already = true
	}
	deadline := time.Now().Add(drainGoneTimeout + killWait)
	for !Gone(stateDir) {
		if time.Now().After(deadline) {
			return r, errors.New("host: the host did not record the command's end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Status, _, err = ReadStatus(stateDir)
	return r, err
}

// endingWait bounds how long a Stop that finds no socket waits for a host
// that runs: one that ends removes its socket and then records the end.
const endingWait = 5 * time.Second

// dialOrEnd is Dial for a Stop. It returns Dial's error once the end of the
// command is recorded, or no host runs that could still record it. A host
// between removing its socket and recording the end is waited for, and so is
// one that is starting and does not listen yet.
func dialOrEnd(stateDir string) (*Client, error) {
	deadline := time.Now().Add(endingWait)
	for {
		runs := hostRuns(stateDir) // before the status is read: it records the end before it goes
		c, err := Dial(stateDir)
		if err == nil || !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return c, err
		}
		if _, ok, _ := ReadStatus(stateDir); ok || !runs || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hostRuns reports whether the process that recorded itself as the host of
// stateDir exists. It does not take the host's lock, which could make a host
// that is starting give up.
func hostRuns(stateDir string) bool {
	b, err := os.ReadFile(filepath.Join(stateDir, HostPIDFile))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
	return err == nil && pid > 1 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// isGoneError reports whether err means the host closed the connection
// while answering, because it was finishing.
func isGoneError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// AttachmentPIDs returns the processes connected by Attach, independently
// of their executable names. An older host or an unidentified connection
// refuses the ownership check instead of exposing a possible agent input.
func (c *Client) AttachmentPIDs(ctx context.Context) ([]int, error) {
	by, ok := ctx.Deadline()
	if !ok {
		by = time.Now().Add(time.Second)
	}
	c.conn.SetDeadline(by)
	defer c.conn.SetDeadline(time.Time{})
	resp, err := c.call(request{Op: "status"})
	if err != nil {
		return nil, err
	}
	if !resp.AttachPIDsKnown || len(resp.AttachPIDs) != resp.Attached {
		return nil, errors.New("host: attached terminal processes cannot be identified")
	}
	for _, pid := range resp.AttachPIDs {
		if pid < 1 {
			return nil, errors.New("host: invalid attached terminal process")
		}
	}
	return resp.AttachPIDs, nil
}
