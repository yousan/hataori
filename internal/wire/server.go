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
	return &Server{API: api, Log: log, uid: os.Getuid(), ln: ln, path: path, conns: map[net.Conn]struct{}{}}, nil
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
	if uid, err := peerUID(c); err != nil || uid != s.uid {
		s.Log.Warn("control connection refused", "peer_uid", uid, "err", err)
		enc.Encode(hello{Version: Version, Error: "connection refused: not the instance's OS user"})
		return
	}
	r := bufio.NewReader(c)
	dec := json.NewDecoder(r)
	var h hello
	if err := dec.Decode(&h); err != nil {
		return
	}
	if h.Version != Version {
		msg := versionError(h.Version, Version).Error()
		s.Log.Warn("control connection refused", "reason", msg)
		enc.Encode(hello{Version: Version, Error: msg})
		return
	}
	if err := enc.Encode(hello{Version: Version}); err != nil {
		return
	}
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.Log.Warn("control connection", "err", err)
			}
			return
		}
		if req.Op == "Events" {
			s.stream(ctx, c, dec, enc, req)
			return
		}
		result, err := s.call(ctx, req)
		resp := response{ID: req.ID}
		if err != nil {
			resp.Error = encodeError(err)
		} else if result != nil {
			resp.Result, _ = json.Marshal(result)
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) call(ctx context.Context, req request) (any, error) {
	args := func(v any) error {
		if err := json.Unmarshal(req.Args, v); err != nil {
			return fmt.Errorf("wire: %s: bad arguments: %w", req.Op, err)
		}
		return nil
	}
	switch req.Op {
	case "Threads":
		var a threadsArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return s.API.Threads(ctx, a.Filter)
	case "Thread":
		var a threadArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return s.API.Thread(ctx, a.ID, a.Recent)
	case "Start":
		var a control.StartRequest
		if err := args(&a); err != nil {
			return nil, err
		}
		return s.API.Start(ctx, a)
	case "Send":
		var a sendArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, s.API.Send(ctx, a.ID, a.Key, a.Input, a.Busy)
	case "Answer":
		var a answerArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, s.API.Answer(ctx, a.Request, a.Answer)
	case "Stop":
		var a stopArgs
		if err := args(&a); err != nil {
			return nil, err
		}
		return nil, s.API.Stop(ctx, a.ID)
	}
	return nil, fmt.Errorf("wire: unknown operation %q", req.Op)
}

// stream sends events until the client goes away.
func (s *Server) stream(ctx context.Context, c net.Conn, dec *json.Decoder, enc *json.Encoder, req request) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	if err := enc.Encode(response{ID: req.ID}); err != nil { // accepted
		return
	}
	go func() { // the client sends nothing more; EOF means it is gone
		io.Copy(io.Discard, c)
		cancel()
	}()
	for e := range evs {
		if err := enc.Encode(response{ID: req.ID, Event: &e}); err != nil {
			return
		}
	}
}
