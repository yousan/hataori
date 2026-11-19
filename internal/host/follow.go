package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// followLimit bounds the output kept for one follower that has not taken
// it yet. A follower that falls further behind is cut off, so a slow reader
// never holds up the command's output or the host's memory.
const followLimit = 1 << 20

// followWriteTimeout bounds one write to a follower's connection.
const followWriteTimeout = 30 * time.Second

// followFirstTimeout bounds how long a follower waits for the host's first
// answer.
const followFirstTimeout = 10 * time.Second

// followFlushTimeout bounds how long the host, once the command has ended
// and the terminal log is closed, waits for followers to be sent the rest.
const followFlushTimeout = 2 * time.Second

// follower is one reader of a boundedLog's new output.
type follower struct {
	mu    sync.Mutex
	buf   []byte // written and not yet taken
	pos   uint64 // the position after buf
	cut   bool   // fell behind; buf is no longer continuous
	ended bool   // the log was closed; nothing more comes
	wake  chan struct{}
}

func (f *follower) notify() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *follower) end() {
	f.mu.Lock()
	f.ended = true
	f.mu.Unlock()
	f.notify()
}

// take returns what was written since the last take.
func (f *follower) take() (data []byte, pos uint64, cut, ended bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, f.buf = f.buf, nil
	return data, f.pos, f.cut, f.ended
}

// deliver hands p, which ends at l.pos, to every follower. It never waits
// for one. l.mu is held.
func (l *boundedLog) deliver(p []byte) {
	if len(p) == 0 {
		return
	}
	for f := range l.followers {
		f.mu.Lock()
		if len(f.buf)+len(p) > followLimit {
			f.cut = true
			delete(l.followers, f)
		} else {
			f.buf = append(f.buf, p...)
			f.pos = l.pos
		}
		f.mu.Unlock()
		f.notify()
	}
}

// follow returns the kept bytes after position after and the position at
// their end, and registers a follower for what is written next. When after
// is not given (ok false), is in the future or is no longer kept, it
// returns the whole kept log with reset set. The follower is nil once the
// log is closed.
func (l *boundedLog) follow(after uint64, ok bool) (data []byte, pos uint64, reset bool, f *follower, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Read under the lock, so that nothing is written between what is read
	// and what the follower gets.
	b, err := os.ReadFile(l.path)
	if err != nil {
		return nil, 0, false, nil, err
	}
	pos = l.pos
	// The file ends at pos; bytes before the start of this count, left by an
	// earlier run, have no position.
	if ok && after <= pos && pos-after <= uint64(len(b)) {
		data = b[uint64(len(b))-(pos-after):]
	} else {
		data, reset = b, true
	}
	if l.closed {
		return data, pos, reset, nil, nil
	}
	f = &follower{pos: pos, wake: make(chan struct{}, 1)}
	if l.followers == nil {
		l.followers = map[*follower]struct{}{}
	}
	l.followers[f] = struct{}{}
	return data, pos, reset, f, nil
}

func (l *boundedLog) unfollow(f *follower) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.followers, f)
}

// newRun returns an id for this run of the host.
func newRun() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// follow serves a "follow" request: it sends the terminal's kept output
// after req.After, or the end of the log marked as a reset, then new output
// as it is written, until the reader closes, falls behind, or the terminal
// is closed. The connection carries no further requests: whatever the
// reader sends is read and dropped.
func (h *host) follow(c *net.UnixConn, enc *json.Encoder, req request) {
	h.stream(c, enc, req, func() { io.Copy(io.Discard, c) }) // never decoded, never passed on
}

// attach serves an "attach" request: the terminal's output as follow sends
// it, and the keys of a person, which the connection then carries as
// "input" requests, written to the terminal as they come and not answered.
// The host counts the connection as attached until it closes.
func (h *host) attach(c *net.UnixConn, dec *json.Decoder, enc *json.Encoder, req request) {
	if h.term == nil {
		enc.Encode(response{Error: "the command runs on pipes: there is no terminal to attach to"})
		return
	}
	h.attached.Add(1)
	defer func() {
		h.detached.Store(time.Now().UnixMilli())
		h.attached.Add(-1)
	}()
	h.log.printf("a terminal attached")
	defer h.log.printf("a terminal detached")
	h.stream(c, enc, req, func() {
		for {
			var in request
			if err := dec.Decode(&in); err != nil {
				return
			}
			if in.Op != "input" || len(in.Data) == 0 {
				continue
			}
			h.inMu.Lock()
			select {
			case <-h.done:
			default:
				h.input.Write(in.Data)
			}
			h.inMu.Unlock()
		}
	})
}

// stream sends the terminal's output after req.After, as follow says,
// while read takes what the other end sends; read returns when the other
// end closes.
func (h *host) stream(c *net.UnixConn, enc *json.Encoder, req request, read func()) {
	if h.term == nil {
		enc.Encode(response{Error: "the command runs on pipes: there is no terminal to follow"})
		return
	}
	data, pos, reset, f, err := h.term.follow(req.After, req.Run == h.run && req.After > 0)
	if err != nil {
		enc.Encode(response{Error: err.Error()})
		return
	}
	if f != nil {
		defer h.term.unfollow(f)
	}
	send := func(r response) bool {
		c.SetWriteDeadline(time.Now().Add(followWriteTimeout))
		if err := enc.Encode(r); err != nil {
			h.log.dropped("writing terminal output to a follower", err)
			return false
		}
		return true
	}
	if !send(response{OK: true, Run: h.run, Pos: pos, Reset: reset, Cols: h.cols, Rows: h.rows, Data: data}) {
		return
	}
	if f == nil {
		send(response{OK: true, Run: h.run, Pos: pos, End: true})
		return
	}
	gone := make(chan struct{})
	go func() {
		read()
		close(gone)
	}()
	for {
		select {
		case <-f.wake:
		case <-gone:
			return
		}
		data, pos, cut, ended := f.take()
		if cut {
			send(response{Error: "the reader fell behind; ask again from the last position"})
			return
		}
		if len(data) > 0 && !send(response{OK: true, Run: h.run, Pos: pos, Data: data}) {
			return
		}
		if ended {
			send(response{OK: true, Run: h.run, Pos: pos, End: true})
			return
		}
	}
}

// KeptTerminal returns the terminal log kept in stateDir, as the last host
// there left it, for a person to look at. It is nil when there is none.
func KeptTerminal(stateDir string) []byte {
	b, _ := os.ReadFile(filepath.Join(stateDir, TerminalLog))
	return b
}

// TerminalChunk is one message of a terminal stream.
type TerminalChunk struct {
	Run   string // the host's run; a new run starts a new count
	Pos   uint64 // the byte position after Data, counted since the run started
	Reset bool   // Data is the end of the log, not a continuation: redraw
	Cols  int    // the terminal's size
	Rows  int
	Data  []byte
}

// TerminalStream follows a host's terminal output. It is not safe for
// concurrent use.
type TerminalStream struct {
	conn  net.Conn
	dec   *json.Decoder
	first *TerminalChunk
	cols  int
	rows  int
}

// FollowTerminal connects to the host in stateDir and follows its terminal
// output after position after of run. The first chunk is the kept output
// after that position, or, when after is 0, belongs to another run or is
// no longer kept, the end of the log with Reset set. Each later chunk
// continues the one before it. The stream sends nothing to the terminal.
func FollowTerminal(stateDir, run string, after uint64) (*TerminalStream, error) {
	c, err := Dial(stateDir)
	if err != nil {
		return nil, err
	}
	s := &TerminalStream{conn: c.conn, dec: c.dec}
	if err := json.NewEncoder(c.conn).Encode(request{Op: "follow", Run: run, After: after}); err != nil {
		c.Close()
		return nil, err
	}
	// The first answer comes at once; a host that does not give it is
	// stuck, and the caller must not wait for it.
	c.conn.SetReadDeadline(time.Now().Add(followFirstTimeout))
	first, err := s.read()
	if err != nil {
		c.Close()
		return nil, err
	}
	c.conn.SetReadDeadline(time.Time{})
	s.first, s.cols, s.rows = &first, first.Cols, first.Rows
	return s, nil
}

// Next returns the next chunk. It returns io.EOF after the terminal was
// closed and everything written to it was sent, and another error when the
// stream ended early, for example because the reader fell behind; the
// reader then follows again from the last position it has.
func (s *TerminalStream) Next() (TerminalChunk, error) {
	if s.first != nil {
		ch := *s.first
		s.first = nil
		return ch, nil
	}
	ch, err := s.read()
	ch.Cols, ch.Rows = s.cols, s.rows
	return ch, err
}

func (s *TerminalStream) read() (TerminalChunk, error) {
	var resp response
	if err := s.dec.Decode(&resp); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF // closed before the end: cut off, or the host died
		}
		return TerminalChunk{}, err
	}
	if resp.Error != "" {
		return TerminalChunk{}, &hostError{resp.Error}
	}
	if resp.End {
		return TerminalChunk{}, io.EOF
	}
	return TerminalChunk{Run: resp.Run, Pos: resp.Pos, Reset: resp.Reset, Cols: resp.Cols, Rows: resp.Rows, Data: resp.Data}, nil
}

// Close ends the stream; the host keeps running.
func (s *TerminalStream) Close() error { return s.conn.Close() }

type hostError struct{ msg string }

func (e *hostError) Error() string { return "host: " + e.msg }

// Attachment is a person's terminal attached to a host's terminal: the
// output as a TerminalStream, and keys passed to the command. While it is
// open the host counts it as attached (Client.Attached).
type Attachment struct {
	*TerminalStream
	mu  sync.Mutex
	enc *json.Encoder
}

// Attach connects to the host listening on the socket sock, as
// SocketPath gives it, and attaches to its terminal. The first chunk is
// the end of the kept log, with Reset set; each later chunk continues it.
func Attach(sock string) (*Attachment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("host: connect: %w", err)
	}
	a := &Attachment{TerminalStream: &TerminalStream{conn: conn, dec: json.NewDecoder(bufio.NewReader(conn))}, enc: json.NewEncoder(conn)}
	if err := a.enc.Encode(request{Op: "attach"}); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(followFirstTimeout))
	first, err := a.read()
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	a.first, a.cols, a.rows = &first, first.Cols, first.Rows
	return a, nil
}

// Input writes b to the command's terminal, unchanged. The host does not
// answer it.
func (a *Attachment) Input(b []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(followWriteTimeout))
	return a.enc.Encode(request{Op: "input", Data: b})
}

// Size is the size of the host's terminal.
func (a *Attachment) Size() (cols, rows int) { return a.cols, a.rows }
