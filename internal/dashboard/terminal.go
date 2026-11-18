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
// The route only sends. Server-Sent Events have no way back, a request body
// or an Upgrade (a WebSocket) on it is dropped and logged, and any other
// method is refused before it gets here: no byte from the page reaches a
// terminal. A thread outside the caller's scopes answers as one that does
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

// dropInput logs and drops whatever a page tried to send on the terminal
// route. Nothing of it is read.
func (s *Server) dropInput(r *http.Request, id control.ThreadID) {
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		s.Log.Warn("dashboard: input to a terminal dropped", "scope", id.Scope, "key", id.Key, "bytes", r.ContentLength)
	}
	if up := r.Header.Get("Upgrade"); up != "" {
		s.Log.Warn("dashboard: upgrade of a terminal stream dropped", "scope", id.Scope, "key", id.Key, "upgrade", up)
	}
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
	q := r.URL.Query()
	id := control.ThreadID{Scope: q.Get("scope"), Key: q.Get("key")}
	s.dropInput(r, id)
	if id.Scope == "" || id.Key == "" {
		http.Error(w, "scope and key are required", http.StatusBadRequest)
		return
	}
	run, after, err := terminalPosition(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stream(s, w, r, func(ctx context.Context) (<-chan control.TerminalChunk, error) {
		return s.API.Terminal(ctx, id, run, after)
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
// then the screen again each time it changes, read every
// second while the page keeps the stream open. A page that
// reconnects gets the current screen. The stream ends when the pane goes.
// It takes no input, as the terminal route does, and a pane the caller may
// not read (host terminals off, or not the owner) answers as one that does
// not exist.
const HostScreenPath = "/api/hostscreen"

// defaultHostScreenInterval is how often an open pane stream reads the
// screen (Server.hostScreenInterval).
const defaultHostScreenInterval = time.Second

type hostScreen struct {
	Screen string `json:"screen"`
}

func (s *Server) hostScreen(w http.ResponseWriter, r *http.Request) {
	pane := r.URL.Query().Get("pane")
	s.dropInput(r, control.ThreadID{Key: pane})
	if pane == "" {
		http.Error(w, "pane is required", http.StatusBadRequest)
		return
	}
	n := 0
	stream(s, w, r, func(ctx context.Context) (<-chan string, error) {
		screen, err := s.API.HostScreen(ctx, pane) // before the answer: a refusal is a 404
		if err != nil {
			return nil, err
		}
		out := make(chan string)
		go func() {
			defer close(out)
			tick := time.NewTicker(s.hostScreenInterval)
			defer tick.Stop()
			for last, first := "", true; ; first = false {
				if screen != last || first {
					select {
					case out <- screen:
					case <-ctx.Done():
						return
					}
					last = screen
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					return
				}
				var err error
				if screen, err = s.API.HostScreen(ctx, pane); err != nil {
					if ctx.Err() == nil && !errors.Is(err, errNotFound) {
						s.Log.Warn("dashboard host screen", "pane", pane, "err", err)
					}
					return
				}
			}
		}()
		return out, nil
	}, s.notFound, func(screen string) message {
		n++
		return message{id: strconv.Itoa(n), data: hostScreen{screen}}
	})
}
