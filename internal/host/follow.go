package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
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
	marks []sizeMark
	wake  chan struct{}
}

// sizeMark is where in a follower's buf the terminal took a new size: the
// bytes from at on are drawn at it.
type sizeMark struct {
	at         int
	cols, rows int
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

// take returns what was written since the last take, and where in it the
// terminal took a new size.
func (f *follower) take() (data []byte, pos uint64, marks []sizeMark, cut, ended bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, f.buf = f.buf, nil
	marks, f.marks = f.marks, nil
	return data, f.pos, marks, f.cut, f.ended
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

// follow returns the kept bytes after position after, the position at
// their end and the terminal's size, and registers a follower for what is
// written next. When after is not given (ok false), is in the future, is
// no longer kept, or comes before the terminal's last resize, it returns
// the whole kept log with reset set: the size of the bytes between is not
// kept, so the reader draws afresh. The follower is nil once the log is
// closed.
func (l *boundedLog) follow(after uint64, ok bool) (data []byte, pos uint64, reset bool, cols, rows int, f *follower, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cols, rows = l.cols, l.rows
	// Read under the lock, so that nothing is written between what is read
	// and what the follower gets.
	b, err := os.ReadFile(l.path)
	if err != nil {
		return nil, 0, false, 0, 0, nil, err
	}
	pos = l.pos
	// The file ends at pos; bytes before the start of this count, left by an
	// earlier run, have no position.
	if ok && after <= pos && pos-after <= uint64(len(b)) && after >= l.resizedAt {
		data = b[uint64(len(b))-(pos-after):]
	} else {
		data, reset = b, true
	}
	if l.closed {
		return data, pos, reset, cols, rows, nil, nil
	}
	f = &follower{pos: pos, wake: make(chan struct{}, 1)}
	if l.followers == nil {
		l.followers = map[*follower]struct{}{}
	}
	l.followers[f] = struct{}{}
	return data, pos, reset, cols, rows, f, nil
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
// It may also carry "resize" requests, also not answered: each gives the
// terminal the size it names, and the agent gets SIGWINCH from the kernel,
// as from a person's terminal emulator; a size out of bounds is dropped.
// The host counts the connection as attached until it closes.
func (h *host) attach(c *net.UnixConn, dec *json.Decoder, enc *json.Encoder, req request) {
	if h.spec.Relay {
		enc.Encode(response{Error: errNoTerminal})
		return
	}
	if h.term == nil {
		enc.Encode(response{Error: "the command runs on pipes: there is no terminal to attach to"})
		return
	}
	// The kernel identifies the peer even when it is an older attach client
	// started through an unrelated link or renamed executable.
	pid, err := peerPID(c)
	if err != nil {
		h.log.dropped("identifying an attached terminal", err)
		pid = 0
	}
	h.attachMu.Lock()
	if h.attachments == nil {
		h.attachments = make(map[*net.UnixConn]int)
	}
	h.attachments[c] = pid
	h.attachMu.Unlock()
	defer func() {
		h.attachMu.Lock()
		delete(h.attachments, c)
		h.detached.Store(time.Now().UnixMilli())
		h.attachMu.Unlock()
	}()
	h.log.printf("a terminal attached")
	// Written by the reader, which stream may leave running when it returns.
	var resized atomic.Pointer[string]
	defer func() {
		if z := resized.Load(); z != nil {
			h.log.printf("a terminal detached; it resized the terminal, last to %s", *z)
		} else {
			h.log.printf("a terminal detached")
		}
	}()
	h.stream(c, enc, req, func() {
		for {
			var in request
			if err := dec.Decode(&in); err != nil {
				return
			}
			if in.Op == "resize" {
				if h.resize(in.Cols, in.Rows) == nil {
					z := fmt.Sprintf("%dx%d", in.Cols, in.Rows)
					resized.Store(&z)
				}
				continue
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

// maxSize bounds the size a "resize" request may give the terminal, in
// either direction.
const maxSize = 1000

// resize gives the terminal cols columns and rows rows, unless the command
// has exited. It does not take the input's lock: a write of keys can wait
// on an agent that waits on its output, which waits on the log's lock that
// resize holds.
func (h *host) resize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > maxSize || rows > maxSize {
		return fmt.Errorf("host: no terminal is %dx%d", cols, rows)
	}
	return h.term.resize(cols, rows, func() error {
		select {
		case <-h.done:
			return errors.New("host: the command has exited")
		default:
		}
		return pty.Setsize(h.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	})
}

// stream sends the terminal's output after req.After, as follow says,
// while read takes what the other end sends; read returns when the other
// end closes.
func (h *host) stream(c *net.UnixConn, enc *json.Encoder, req request, read func()) {
	if h.spec.Relay {
		enc.Encode(response{Error: errNoTerminal})
		return
	}
	if h.term == nil {
		enc.Encode(response{Error: "the command runs on pipes: there is no terminal to follow"})
		return
	}
	data, pos, reset, cols, rows, f, err := h.term.follow(req.After, req.Run == h.run && req.After > 0)
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
	if !send(response{OK: true, Run: h.run, Pos: pos, Reset: reset, Cols: cols, Rows: rows, Data: data}) {
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
		data, pos, marks, cut, ended := f.take()
		if cut {
			send(response{Error: "the reader fell behind; ask again from the last position"})
			return
		}
		// The bytes before each new size, then the size, alone: the reader
		// draws what follows it at that size.
		start, from := pos-uint64(len(data)), 0
		for _, m := range marks {
			if m.at > from && !send(response{OK: true, Run: h.run, Pos: start + uint64(m.at), Data: data[from:m.at]}) {
				return
			}
			if !send(response{OK: true, Run: h.run, Pos: start + uint64(m.at), Cols: m.cols, Rows: m.rows}) {
				return
			}
			from = m.at
		}
		if len(data) > from && !send(response{OK: true, Run: h.run, Pos: pos, Data: data[from:]}) {
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

// Next returns the next chunk. Each carries the terminal's size; a chunk
// whose size differs from the one before it, which may have no data, says
// the terminal was resized there, and the data after it is drawn at the
// new size. It returns io.EOF after the terminal was closed and everything
// written to it was sent, and another error when the stream ended early,
// for example because the reader fell behind; the reader then follows
// again from the last position it has.
func (s *TerminalStream) Next() (TerminalChunk, error) {
	if s.first != nil {
		ch := *s.first
		s.first = nil
		return ch, nil
	}
	ch, err := s.read()
	if err == nil && ch.Cols > 0 && ch.Rows > 0 {
		s.cols, s.rows = ch.Cols, ch.Rows
	}
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

	reading sync.Mutex // held by Next, and by Close while it reads to the end
	closed  bool       // Close read to the end; under reading
}

// Next is TerminalStream.Next; once Close reads to the end it returns
// net.ErrClosed.
func (a *Attachment) Next() (TerminalChunk, error) {
	a.reading.Lock()
	defer a.reading.Unlock()
	if a.closed {
		return TerminalChunk{}, net.ErrClosed
	}
	return a.TerminalStream.Next()
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

// Resize gives the host's terminal cols columns and rows rows, as a
// person's terminal emulator does when its window changes size. The host
// does not answer it; the stream's next chunks carry the new size.
func (a *Attachment) Resize(cols, rows int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(followWriteTimeout))
	return a.enc.Encode(request{Op: "resize", Cols: cols, Rows: rows})
}

// Close ends the attachment; the host keeps running. What Input and Resize
// wrote before reaches the host: it is told that nothing more comes, and
// its output is read until it closes, as it does once it has read all. A
// connection closed under it would lose what it had not read yet: its next
// write of output fails and it drops the connection, and output left unread
// here resets it. A host that does not close in closeWait, as when its
// command takes no input, loses the rest.
func (a *Attachment) Close() error {
	if c, ok := a.conn.(*net.UnixConn); ok && c.CloseWrite() == nil {
		c.SetReadDeadline(time.Now().Add(closeWait))
		a.reading.Lock() // a Next under way returns with what comes, or at the deadline
		a.closed = true
		io.Copy(io.Discard, c)
		a.reading.Unlock()
	}
	return a.conn.Close()
}

// closeWait bounds how long closing an attachment waits for the host to
// read what was written to it.
const closeWait = 2 * time.Second

// Size is the size of the host's terminal, as the stream last carried it.
func (a *Attachment) Size() (cols, rows int) { return a.cols, a.rows }
