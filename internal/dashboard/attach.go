package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	hsession "github.com/yousan/hataori/internal/session"
)

// Typing into a session's terminal from its tab: `hataori attach` in the
// browser (design, section 7.2, "Giving a thread input from its tab"). The
// page opens a WebSocket of its own; the server calls AttachTerminal as the
// session's person, which only the owner may, attaches to the session
// host's terminal at the socket it names, as the command does, and writes
// the page's keys to the terminal as they come. The page keeps drawing the
// terminal from TerminalPath; nothing of the terminal comes back on this
// connection. While it is attached the page may give the terminal its
// pane's size, which the session host sets as a person's terminal emulator
// would; the new size reaches every viewer in the terminal's stream. Viewing
// never resizes it.
//
//	GET /api/attach   a WebSocket; its first message is {"session", "thread"}
//
// After the start the server sends {"attached": {"idle": <seconds>}}. The
// page sends {"keys": "<text>"}, the bytes its terminal emulator made of the
// person's keys and pastes, written in order, {"resize": {"cols", "rows"}},
// the size of the page's pane in cells, and {"end": true}. A size outside
// MinCols×MinRows to MaxCols×MaxRows is answered with an error and changes
// nothing. When the attachment ends the server sends {"ended": "<reason>"}
// and closes: "ended" (the page asked), "idle" (no key for the idle
// period), "session ended" (the dashboard session expired), "agent ended"
// (the session's host ended the attachment, as when the agent ends) or
// "stopping" (the daemon stops).
// A refusal of the start sends {"refused": "<reason>"} and closes with it.
// Closing the WebSocket ends the attachment at once.
//
// The handshake needs the listener's exact Origin, as the operate
// connection's does, and carries no session: the session is the first
// message. On the web listener the start needs the check that acting
// needs (act.go). Each start and end is one log line with the person, the
// thread and, for the end, the reason; an attachment that resized the
// terminal logs one more line at its end, with the last size it gave. What
// is typed is not logged, nor how much.
const AttachPath = "/api/attach"

// TerminalInput is a terminal attached to a session's terminal, as the
// session host serves `hataori attach`: while it is open the host counts it
// as attached, and the runner does not put the session to sleep.
type TerminalInput interface {
	// Input writes b to the agent's terminal, unchanged.
	Input(b []byte) error
	// Resize gives the agent's terminal cols columns and rows rows.
	Resize(cols, rows int) error
	// Wait reads, and drops, what the host sends on the attachment, until
	// it ends: it returns when the host ends it or the attachment is
	// closed.
	Wait() error
	Close() error
}

// DefaultAttachIdle is how long an attachment lasts without a key when the
// config names no idle period.
const DefaultAttachIdle = 5 * time.Minute

// attachFirst is the attachment WebSocket's first message.
type attachFirst struct {
	Session string           `json:"session"`
	Thread  control.ThreadID `json:"thread"`
}

// attachIn is every later message of the page.
type attachIn struct {
	Keys   string      `json:"keys,omitempty"`
	Resize *attachSize `json:"resize,omitempty"`
	End    bool        `json:"end,omitempty"`
}

// attachSize is a size of the session's terminal, in cells.
type attachSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// The sizes a page may give a session's terminal: those the config's
// terminal_size may name (internal/instance). A pane smaller or larger is
// drawn scaled, as when viewing.
const (
	MinCols, MaxCols = 40, 500
	MinRows, MaxRows = 10, 200
)

// attachOut is a message to the page.
type attachOut struct {
	Attached *struct {
		Idle int64 `json:"idle"` // seconds
	} `json:"attached,omitempty"`
	Error   string `json:"error,omitempty"`
	Ended   string `json:"ended,omitempty"`
	Refused string `json:"refused,omitempty"`
}

func (s *Server) attachRoutes() {
	s.mux.HandleFunc("GET "+AttachPath, s.attach)
}

// attachRefusal is what the page is told when AttachTerminal refuses.
func attachRefusal(err error) string {
	switch {
	case errors.Is(err, control.ErrForbidden):
		return "you may not type into this session's terminal: only the owner may"
	case errors.Is(err, core.ErrNotFound):
		return "no such thread"
	case errors.Is(err, control.ErrClosed):
		return "the thread is closed"
	case errors.Is(err, core.ErrNoTerminal):
		return "this session has no terminal: send it a message instead"
	case errors.Is(err, hsession.ErrRefused):
		return "this session cannot be attached now: it has no agent running"
	}
	return "the terminal cannot be attached now"
}

// attach serves the WebSocket of one attachment.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	l := listenerOfRequest(r)
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader answered
	}
	c := &wsConn{Conn: ws, timeout: s.writeTimeout}
	defer c.Close()
	// Until it is attached, a server that closes closes the connection,
	// and ends a wake that AttachTerminal waits for: the connection is the
	// handler's, not the http.Server's.
	setup, cancelSetup := context.WithCancel(r.Context())
	defer cancelSetup()
	go func() {
		select {
		case <-s.closing:
			cancelSetup()
		case <-setup.Done():
		}
	}()
	stopClosing := context.AfterFunc(setup, func() { c.Close() })
	c.SetReadLimit(maxPasskeyBody)
	c.SetReadDeadline(time.Now().Add(firstMessageTimeout))
	var first attachFirst
	kind, b, err := c.ReadMessage()
	if err != nil {
		return
	}
	if kind != websocket.TextMessage || json.Unmarshal(b, &first) != nil {
		c.refuseWith("the first message must be the session")
		return
	}
	c.SetReadDeadline(time.Time{})
	claims, ok := s.valid(first.Session, l)
	var a control.API
	if ok {
		a, ok = s.For(claims.person)
	}
	if !ok || a == nil {
		c.refuseWith("not signed in")
		return
	}
	if first.Thread <= 0 {
		c.refuseWith("thread is required: the thread's number")
		return
	}
	ss := session{claims: claims, sum: sha256.Sum256([]byte(first.Session)), api: a}
	if reason, _, err := s.actRefusal(ss, l); err != nil || reason != "" {
		if err != nil {
			s.Log.Error("dashboard: passkeys", "err", err)
			reason = "the passkeys cannot be read now"
		}
		c.refuseWith(reason)
		return
	}
	if s.attachHost == nil {
		c.refuseWith("typing into a session's terminal is not served here")
		return
	}

	// The attachment lasts at most as long as the session, and ends when
	// the WebSocket closes.
	ctx, cancel := context.WithDeadline(r.Context(), claims.exp)
	defer cancel()
	at, err := a.AttachTerminal(setup, first.Thread)
	if err != nil {
		c.refuseWith(attachRefusal(err))
		return
	}
	term, err := s.attachHost(at.Socket)
	if err != nil {
		s.Log.Warn("dashboard: attaching a terminal", "person", claims.person, "thread", int64(first.Thread), "err", err)
		c.refuseWith("the session's terminal cannot be reached now")
		return
	}
	if !stopClosing() { // the server closed while it attached
		term.Close()
		return
	}
	// Once the end begins no key is written: the end closes the terminal
	// under writing, before the page is told.
	var (
		writing sync.Mutex
		over    bool
	)
	closeTerm := func() {
		writing.Lock()
		defer writing.Unlock()
		if !over {
			over = true
			term.Close()
		}
	}
	defer closeTerm()
	idle := s.attachIdle
	s.Log.Info("dashboard: terminal attached", "person", claims.person, "thread", int64(first.Thread), "listener", string(l.way))
	reason := "closed"
	var resized atomic.Pointer[attachSize] // the last size given, if any
	defer func() {
		s.Log.Info("dashboard: terminal detached", "person", claims.person, "thread", int64(first.Thread), "reason", reason)
		if z := resized.Load(); z != nil {
			s.Log.Info("dashboard: terminal resized", "person", claims.person, "thread", int64(first.Thread), "cols", z.Cols, "rows", z.Rows)
		}
	}()
	c.SetReadLimit(maxOperateMessage)
	alive := func() { c.SetReadDeadline(time.Now().Add(2*s.keepAlive + s.writeTimeout)) }
	alive()
	c.SetPongHandler(func(string) error { alive(); return nil })
	if c.send(attachOut{Attached: &struct {
		Idle int64 `json:"idle"`
	}{int64(idle / time.Second)}}) != nil {
		return
	}

	hostEnded := make(chan error, 1)
	go func() { hostEnded <- term.Wait() }()
	// The reader writes each batch of keys to the terminal in order, and
	// records when the last one was written: the idle period runs from it.
	var lastKey atomic.Int64
	lastKey.Store(time.Now().UnixNano())
	pageEnded := make(chan string, 1) // why the page's side ended
	go func() {
		for {
			kind, b, err := c.ReadMessage()
			if err != nil {
				pageEnded <- "closed"
				return
			}
			var m attachIn
			if kind != websocket.TextMessage || json.Unmarshal(b, &m) != nil {
				// The message of a decoding error may quote the text.
				c.send(attachOut{Error: "not a message of an attachment: nothing was sent"})
				continue
			}
			if m.End {
				pageEnded <- "ended"
				return
			}
			if z := m.Resize; z != nil {
				if z.Cols < MinCols || z.Cols > MaxCols || z.Rows < MinRows || z.Rows > MaxRows {
					c.send(attachOut{Error: fmt.Sprintf("a terminal of %dx%d is not given: it must be from %dx%d to %dx%d", z.Cols, z.Rows, MinCols, MinRows, MaxCols, MaxRows)})
					continue
				}
				writing.Lock()
				if over {
					writing.Unlock()
					return
				}
				err = term.Resize(z.Cols, z.Rows)
				writing.Unlock()
				if err != nil {
					c.send(attachOut{Error: "the terminal could not be resized"})
					continue
				}
				resized.Store(&attachSize{z.Cols, z.Rows})
				continue
			}
			if m.Keys == "" {
				continue
			}
			writing.Lock()
			if over {
				writing.Unlock()
				return // ended: nothing more is written
			}
			err = term.Input([]byte(m.Keys))
			if err == nil {
				lastKey.Store(time.Now().UnixNano())
			}
			writing.Unlock()
			if err != nil {
				c.send(attachOut{Error: "the keys could not be written to the terminal"})
			}
		}
	}()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	pings := time.NewTicker(s.keepAlive)
	defer pings.Stop()
	end := func(why string) {
		reason = why
		closeTerm()
		if why != "closed" {
			c.send(attachOut{Ended: why})
			c.close(websocket.CloseNormalClosure, why)
		}
	}
	for {
		select {
		case <-timer.C:
			if left := idle - time.Since(time.Unix(0, lastKey.Load())); left > 0 {
				timer.Reset(left) // a key came since the timer was set
				continue
			}
			end("idle")
			return
		case why := <-pageEnded:
			end(why)
			return
		case <-hostEnded:
			end("agent ended")
			return
		case <-s.closing:
			end("stopping")
			return
		case <-ctx.Done():
			end("session ended")
			return
		case <-pings.C:
			c.mu.Lock()
			err := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(s.writeTimeout))
			c.mu.Unlock()
			if err != nil {
				end("closed")
				return
			}
		}
	}
}

// refuseWith tells the page why the start is refused and closes.
func (c *wsConn) refuseWith(reason string) {
	c.send(attachOut{Refused: reason})
	c.close(websocket.ClosePolicyViolation, strings.TrimSpace(reason))
}
