package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

// SocketPath is derived from operator configuration, never from a report or a
// main resource address. The short fallback has an owned, private parent.
func SocketPath(root string) string {
	p := filepath.Join(root, "state/worker/resident.sock")
	if len(p) <= 100 {
		return p
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join("/tmp", "hataori-worker-"+hex.EncodeToString(sum[:12]), "resident.sock")
}

func (r *Resident) Hello() workerproto.Hello {
	return workerproto.Hello{Version: workerproto.Version, Binding: r.binding,
		Capabilities: []string{"health", "host", "receipts", "lifecycle", "workspace", "transfer", "runner", "replay", "terminal", "placement", workerproto.LoginCapability}, Required: []string{"receipts", "lifecycle"}}
}

func (r *Resident) Listen() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", errors.New("worker: resident is closed")
	}
	if r.listener != nil {
		return "", errors.New("worker: already listening")
	}
	p := SocketPath(r.dir)
	parent := filepath.Dir(p)
	if parent != filepath.Join(r.dir, "state/worker") {
		if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(parent)
		if err != nil {
			return "", err
		}
		if err := private(info, true); err != nil {
			return "", err
		}
	}
	if info, err := os.Lstat(p); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("worker: refusing to replace an unowned/non-socket destination")
		}
		c, e := net.DialTimeout("unix", p, 250*time.Millisecond)
		if e == nil {
			c.Close()
			return "", errors.New("worker: another endpoint is listening")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) && !errors.Is(e, os.ErrNotExist) {
			return "", errors.New("worker: existing endpoint cannot be classified")
		}
		if current, e := os.Lstat(p); e != nil || !os.SameFile(current, info) {
			return "", errors.New("worker: socket identity changed")
		}
		if err := os.Remove(p); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Do not change the process-wide umask: it is shared with host starts and
	// other goroutines. The parent is already private before Listen creates it.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		return "", err
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(p, 0o600); err != nil {
		ln.Close()
		return "", err
	}
	info, err := os.Lstat(p)
	if err != nil {
		ln.Close()
		return "", err
	}
	r.listener, r.socket, r.socketInfo = ln, p, info
	return p, nil
}

// Serve runs until its context ends. Releasing the socket/lock never stops
// detached session hosts or answers their waiting hooks.
func (r *Resident) Serve(ctx context.Context) error {
	r.mu.Lock()
	ln := r.listener
	r.mu.Unlock()
	if ln == nil {
		return errors.New("worker: Listen must precede Serve")
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			r.Close()
		case <-done:
		}
	}()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		r.mu.Lock()
		if r.closed || len(r.connections) >= 64 {
			r.mu.Unlock()
			c.Close()
			continue
		}
		r.connections[c] = true
		r.mu.Unlock()
		go r.serveConnection(ctx, c)
	}
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	b, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(b) > workerproto.MaxFrame {
		return nil, errors.New("worker: frame limit")
	}
	return b, nil
}

func sendFrame(c net.Conn, v any) error {
	b, err := workerproto.Encode(v)
	if err != nil {
		return err
	}
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	for len(b) > 0 {
		n, err := c.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func (r *Resident) serveConnection(ctx context.Context, c *net.UnixConn) {
	defer func() { c.Close(); r.mu.Lock(); delete(r.connections, c); r.mu.Unlock() }()
	reader := bufio.NewReaderSize(c, workerproto.MaxFrame)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err := readFrame(reader)
	if err != nil {
		return
	}
	peer, err := workerproto.DecodeHello(data)
	if err != nil {
		return
	}
	connection, err := workerproto.Negotiate(r.binding, r.Hello(), peer)
	if err != nil {
		var refusal *workerproto.VersionRefusal
		if errors.As(err, &refusal) {
			sendFrame(c, refusal)
		} else {
			var refused *workerproto.Refusal
			if errors.As(err, &refused) {
				sendFrame(c, refused)
			}
		}
		return
	}
	if sendFrame(c, connection.Hello()) != nil {
		return
	}
	for {
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		data, err := readFrame(reader)
		if err != nil {
			return
		}
		var route struct {
			Op string `json:"op"`
		}
		json.Unmarshal(data, &route)
		if route.Op == workerproto.LoginStart {
			r.serveLogin(ctx, c, reader, connection, data)
			return
		}
		if route.Op == "attach" {
			r.serveAttachment(ctx, c, reader, connection, data)
			return
		}
		reply := r.Dispatch(ctx, connection, data)
		if sendFrame(c, reply) != nil {
			return
		}
	}
}
