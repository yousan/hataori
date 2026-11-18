package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Server serves the control API on a Unix socket. Every connection is
// made as the instance's owner, and only the OS user that runs the daemon
// may connect.
type Server struct {
	API control.API // bound to the owner's Caller
	Log *slog.Logger

	uid   int // the OS user allowed to connect
	ln    *net.UnixListener
	path  string
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	peers map[*Peer]struct{}
}

// Listen creates the socket for the instance at root and returns a server
// that is ready to Serve.
func Listen(root string, api control.API, log *slog.Logger) (*Server, error) {
	path, err := SocketPath(root)
	if err != nil {
		return nil, err
	}
	pointer := filepath.Join(root, pointerFile)
	if path != filepath.Join(root, SocketFile) {
		if err := privateDir(filepath.Dir(path)); err != nil {
			return nil, err
		}
		if err := os.WriteFile(pointer, []byte(path+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("wire: record socket path: %w", err)
		}
	} else if err := os.Remove(pointer); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wire: %w", err)
	}
	// The instance lock is held, so a socket file left here is stale.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wire: remove stale socket: %w", err)
	}
	old := syscall.Umask(0o177) // the socket is created 0600
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("wire: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("wire: %w", err)
	}
	return &Server{API: api, Log: log, uid: os.Getuid(), ln: ln, path: path, conns: map[net.Conn]struct{}{}, peers: map[*Peer]struct{}{}}, nil
}

// privateDir makes dir, owned by this user and closed to others.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("wire: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("wire: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("wire: %s is not a private directory of this user", dir)
	}
	return nil
}

// Path returns the socket's path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until ctx ends or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return fmt.Errorf("wire: accept: %w", err)
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go func() {
			s.serveConn(ctx, c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			c.Close()
		}()
	}
}

// Close stops listening, closes every connection and removes the socket.
func (s *Server) Close() error {
	err := s.ln.Close()
	s.DropConns()
	return err
}

// DropConns closes every open connection, as if the daemon had restarted.
func (s *Server) DropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
}

func (s *Server) serveConn(ctx context.Context, c *net.UnixConn) {
	enc := json.NewEncoder(c)
	// Read the client's hello before answering, so that a refusal reaches a
	// client that is still writing it. The hello must come quickly and be
	// short.
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	dec := json.NewDecoder(io.LimitReader(c, 4096))
	var h hello
	err := dec.Decode(&h)
	c.SetReadDeadline(time.Time{})
	if uid, perr := peerUID(c); perr != nil || uid != s.uid {
		s.Log.Warn("control connection refused", "peer_uid", uid, "err", perr)
		enc.Encode(hello{Version: Version, Error: "connection refused: not the instance's OS user"})
		return
	}
	if err != nil {
		return
	}
	// Whatever the hello's decoder read ahead belongs to the requests.
	dec = json.NewDecoder(bufio.NewReader(io.MultiReader(dec.Buffered(), c)))
	if major(h.Version) != major(Version) {
		msg := versionError(h.Version, Version).Error()
		s.Log.Warn("control connection refused", "reason", msg)
		enc.Encode(hello{Version: Version, Error: msg})
		return
	}
	if err := enc.Encode(hello{Version: Version}); err != nil {
		return
	}
	p := newPeer(c, func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		return s.call(ctx, request{Op: op, Args: args})
	})
	s.mu.Lock()
	s.peers[p] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.peers, p)
		s.mu.Unlock()
	}()
	err = p.serve(ctx, dec, func(req request) bool {
		if req.Op != "Events" && req.Op != "Terminal" {
			return false
		}
		s.mu.Lock()
		delete(s.peers, p) // the connection is a stream now
		s.mu.Unlock()
		s.stream(ctx, c, dec, enc, req)
		return true
	})
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.Log.Warn("control connection", "err", err)
	}
}

// Peers returns the clients connected now, to send them requests. A
// connection that carries an event stream is not among them.
func (s *Server) Peers() []*Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Peer
	for p := range s.peers {
		out = append(out, p)
	}
	return out
}

func (s *Server) call(ctx context.Context, req request) (any, error) {
	op, ok := operations[req.Op]
	if !ok || op.run == nil {
		return nil, fmt.Errorf("wire: operation %q: %w", req.Op, ErrNotSupported)
	}
	return op.run(ctx, s.API, func(v any) error {
		if err := json.Unmarshal(req.Args, v); err != nil {
			return fmt.Errorf("wire: %s: bad arguments: %w", req.Op, err)
		}
		return nil
	})
}

// stream sends events, or terminal output, until the client goes away.
// Whatever the client sends on the stream is read and dropped.
func (s *Server) stream(ctx context.Context, c net.Conn, dec *json.Decoder, enc *json.Encoder, req request) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next func() (response, bool)
	switch req.Op {
	case "Events":
		var a eventsArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			enc.Encode(response{ID: req.ID, Error: encodeError(err)})
			return
		}
		evs, err := s.API.Events(ctx, a.Filter, a.After)
		if err != nil {
			enc.Encode(response{ID: req.ID, Error: encodeError(err)})
			return
		}
		next = func() (response, bool) {
			e, ok := <-evs
			return response{ID: req.ID, Event: &e}, ok
		}
	case "Terminal":
		var a terminalArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			enc.Encode(response{ID: req.ID, Error: encodeError(err)})
			return
		}
		chunks, err := s.API.Terminal(ctx, a.ID, a.Run, a.After)
		if err != nil {
			enc.Encode(response{ID: req.ID, Error: encodeError(err)})
			return
		}
		next = func() (response, bool) {
			ch, ok := <-chunks
			return response{ID: req.ID, Terminal: &ch}, ok
		}
	}
	if err := enc.Encode(response{ID: req.ID}); err != nil { // accepted
		return
	}
	go func() { // the client sends nothing more; EOF means it is gone
		io.Copy(io.Discard, c)
		cancel()
	}()
	for {
		resp, ok := next()
		if !ok {
			return
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}
