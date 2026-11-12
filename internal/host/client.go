package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
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
