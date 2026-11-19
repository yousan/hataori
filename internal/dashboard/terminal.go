package dashboard

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// TerminalPath is the route of a session's live terminal:
//
//	GET /api/terminal?scope=S&key=K
//
// It is a Server-Sent Events stream of the thread's TerminalChunk, in the
// protocol's JSON (Data in base64). The first message is the end of the
// kept log with reset set, unless the page resumes; then output follows in
// order. Each message's id is "<pos>:<run>", so a page that reconnects with
// Last-Event-ID goes on after the last byte it drew, or gets a reset when
// that is no longer kept.
//
// The route only sends. Server-Sent Events have no way back; a request
// with a body or an Upgrade (a WebSocket) is logged and refused unread,
// before anything is opened, and any other method is refused before it gets
// here: no byte from the page reaches a terminal. A thread outside the caller's scopes answers as one that does
// not exist.
const TerminalPath = "/api/terminal"

// terminalPosition is where a terminal stream resumes: the run and position
// of the Last-Event-ID a reconnecting page sends, else from a reset.
func terminalPosition(r *http.Request) (run string, after uint64, err error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		return "", 0, nil
	}
	p, run, ok := strings.Cut(v, ":")
	if !ok {
		return "", 0, errors.New("not a terminal position")
	}
	if after, err = strconv.ParseUint(p, 10, 64); err != nil {
		return "", 0, errors.New("not a terminal position")
	}
	return run, after, nil
}

// dropInput refuses a request that tries to send something on a terminal
// route, and logs it. The body is not read, and the connection is closed
// after the answer, so a body sent in part holds nothing open.
func (s *Server) dropInput(w http.ResponseWriter, r *http.Request, what string) bool {
	body := r.ContentLength != 0 || len(r.TransferEncoding) > 0
	up := r.Header.Get("Upgrade")
	if !body && up == "" {
		return false
	}
	if body {
		s.Log.Warn("dashboard: input to a terminal dropped", "terminal", what, "bytes", r.ContentLength)
	}
	if up != "" {
		s.Log.Warn("dashboard: upgrade of a terminal stream dropped", "terminal", what, "upgrade", up)
	}
	// The server would otherwise wait for the rest of the body to drain it.
	http.NewResponseController(w).SetReadDeadline(time.Now())
	w.Header().Set("Connection", "close")
	http.Error(w, "this route takes no input", http.StatusBadRequest)
	return true
}

// notFound answers every failure to open a terminal the same way, so that
// a thread outside the caller's scopes cannot be told from no thread.
func (s *Server) notFound(w http.ResponseWriter, err error) {
	if !errors.Is(err, control.ErrForbidden) && !errors.Is(err, errNotFound) {
		s.Log.Warn("dashboard terminal", "err", err)
	}
	http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	if s.dropInput(w, r, "thread "+r.URL.Query().Get("thread")) {
		return
	}
	id, err := threadParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	run, after, err := terminalPosition(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stream(s, w, r, func(ctx context.Context) (<-chan control.TerminalChunk, error) {
		return api(r).Terminal(ctx, id, run, after)
	}, s.notFound, func(c control.TerminalChunk) message {
		return message{id: strconv.FormatUint(c.Pos, 10) + ":" + c.Run, data: c}
	})
}

// HostScreenPath is the route of a tmux pane that hataori does not own:
//
//	GET /api/hostscreen?pane=P
//
// P is the pane's id, as HostTerminals gives it (TmuxPane.Pane). It is a
// Server-Sent Events stream of {"screen": "..."}: the pane's whole screen,
// then the screen again each time it changes, read every second while the
// page keeps the stream open. While the pane is operated through this
// server (operate.go) it is read five times a second instead, and each
// message also carries the cursor, {"screen": "...", "cursor": {"x", "y"}},
// so that what is typed shows within a fifth of a second; a message is
// sent when the screen or the cursor changes. A page that reconnects gets
// the current screen. The stream ends when the pane goes. It takes no
// input, as the terminal route does, and a pane the caller may not read
// (host terminals off, or not the owner) answers as one that does not
// exist.
const HostScreenPath = "/api/hostscreen"

// defaultHostScreenInterval is how often an open pane stream reads the
// screen (Server.hostScreenInterval).
const defaultHostScreenInterval = time.Second

type hostScreen struct {
	Screen string  `json:"screen"`
	Cursor *cursor `json:"cursor,omitempty"` // while the pane is operated
}

type cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// readScreen reads pane once: with its cursor while it is operated through
// this server.
func (s *Server) readScreen(ctx context.Context, a control.API, pane string) (hostScreen, error) {
	if !s.isOperated(pane) {
		screen, err := a.HostScreen(ctx, pane)
		return hostScreen{Screen: screen}, err
	}
	v, err := a.HostView(ctx, pane)
	return hostScreen{Screen: v.Screen, Cursor: &cursor{v.CursorX, v.CursorY}}, err
}

func (s *Server) hostScreen(w http.ResponseWriter, r *http.Request) {
	pane := r.URL.Query().Get("pane")
	if s.dropInput(w, r, "pane "+pane) {
		return
	}
	if pane == "" {
		http.Error(w, "pane is required", http.StatusBadRequest)
		return
	}
	n := 0
	a := api(r)
	stream(s, w, r, func(ctx context.Context) (<-chan hostScreen, error) {
		screen, err := s.readScreen(ctx, a, pane) // before the answer: a refusal is a 404
		if err != nil {
			return nil, err
		}
		out := make(chan hostScreen)
		go func() {
			defer close(out)
			next := time.NewTimer(0)
			defer next.Stop()
			<-next.C
			var last hostScreen
			for first := true; ; first = false {
				if first || !sameScreen(screen, last) {
					select {
					case out <- screen:
					case <-ctx.Done():
						return
					}
					last = screen
				}
				changed := s.operatedChange() // before the rate is chosen, so that no change is missed
				every := s.hostScreenInterval
				if s.isOperated(pane) {
					every = min(every, s.operatedScreenInterval)
				}
				next.Reset(every)
				select {
				case <-next.C:
				case <-changed: // an operation started or ended: read now, at its rate
					if !next.Stop() {
						<-next.C
					}
				case <-ctx.Done():
					return
				}
				var err error
				if screen, err = s.readScreen(ctx, a, pane); err != nil {
					if ctx.Err() == nil && !errors.Is(err, errNotFound) {
						s.Log.Warn("dashboard host screen", "pane", pane, "err", err)
					}
					return
				}
			}
		}()
		return out, nil
	}, s.notFound, func(screen hostScreen) message {
		n++
		return message{id: strconv.Itoa(n), data: screen}
	})
}

// sameScreen reports whether a and b show the same, cursor included.
func sameScreen(a, b hostScreen) bool {
	return a.Screen == b.Screen && (a.Cursor == nil) == (b.Cursor == nil) && (a.Cursor == nil || *a.Cursor == *b.Cursor)
}
