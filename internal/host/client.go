package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
	c, err := net.DialTimeout("unix", SocketPath(stateDir), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("host: connect: %w", err)
	}
	return &Client{conn: c, dec: json.NewDecoder(bufio.NewReader(c))}, nil
}

// Close closes the connection; the host keeps running.
func (c *Client) Close() error { return c.conn.Close() }

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

// PID returns the command's process id.
func (c *Client) PID() (int, error) {
	resp, err := c.call(request{Op: "status"})
	return resp.PID, err
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

// StopResult says what Stop did.
type StopResult struct {
	Already bool   // the command had already exited; nothing was sent
	Signals string // the signals sent, in order, such as "SIGINT,SIGTERM"
	Status  Status // how the command ended, as recorded
}

// Stop ends the command of the host in stateDir and leaves no process of
// its group: SIGINT, then SIGTERM after interrupt, then SIGKILL after
// terminate. It returns once the host has recorded the command's status,
// so its logs are complete. A command that already exited is not an error.
func Stop(stateDir string, interrupt, terminate time.Duration) (StopResult, error) {
	var r StopResult
	c, err := Dial(stateDir)
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

// isGoneError reports whether err means the host closed the connection
// while answering, because it was finishing.
func isGoneError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
