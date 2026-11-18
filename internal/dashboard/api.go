package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// The routes the page reads. Each passes on what the control API returns to
// the server's Caller, in the protocol's JSON (docs/protocol.md), and adds
// nothing: the core decides which threads, events and text fields the
// caller may see.
//
//	GET /api/threads                          [ThreadInfo]
//	GET /api/thread?scope=S&key=K[&recent=N][&events=N]   ThreadDetail
//	GET /api/events[?after=N]                 a stream of Event (Server-Sent Events)
//	GET /api/terminal?scope=S&key=K           a stream of TerminalChunk (Server-Sent Events)
//	GET /api/hostscreen?pane=P                a stream of a tmux pane's screen (Server-Sent Events)
//	GET /api/limits                           [AgentLimits]
//	GET /api/workspace?scope=S&key=K          WorkspaceState
//
// The event stream sends each event as one message whose id is the event's
// position, and the caught_up mark as a message of type caught_up. A page
// that reconnects sends the last id it saw as Last-Event-ID, as a browser's
// EventSource does, and gets every later event once. The terminal and
// pane streams are described in terminal.go.
const (
	ThreadsPath = "/api/threads"
	ThreadPath  = "/api/thread"
	EventsPath  = "/api/events"
	LimitsPath  = "/api/limits"
	// WorkspacePath answers with the thread's WorkspaceState. A thread
	// outside the caller's scopes answers as one that does not exist, and a
	// workspace that cannot be read is an error, never zeros.
	WorkspacePath = "/api/workspace"
)

// Defaults and caps of /api/thread.
const (
	defaultRecent = 4096
	maxRecent     = 64 << 10
	defaultEvents = 20
)

// Defaults of the stream timings (Server.keepAlive, writeTimeout).
const (
	// defaultKeepAlive is how often an idle stream sends a comment, so that
	// a proxy or a browser does not close it.
	defaultKeepAlive = 15 * time.Second
	// defaultWriteTimeout is how long one write to a stream may take. A
	// page that stops reading loses its stream, and the follower behind it
	// ends.
	defaultWriteTimeout = 30 * time.Second
)

// writeBy is the deadline of one write to a stream: writeTimeout from now,
// and never after the session expires.
func (s *Server) writeBy(exp time.Time) time.Time {
	d := time.Now().Add(s.writeTimeout)
	if exp.Before(d) {
		return exp
	}
	return d
}

// errNotFound is the core's error for no such thread or session.
var errNotFound = core.ErrNotFound

func (s *Server) routes() {
	s.mux.HandleFunc("GET "+ThreadsPath, s.threads)
	s.mux.HandleFunc("GET "+ThreadPath, s.thread)
	s.mux.HandleFunc("GET "+EventsPath, s.events)
	s.mux.HandleFunc("GET "+TerminalPath, s.terminal)
	s.mux.HandleFunc("GET "+HostScreenPath, s.hostScreen)
	s.mux.HandleFunc("GET "+LimitsPath, s.limits)
	s.mux.HandleFunc("GET "+WorkspacePath, s.workspace)
}

func (s *Server) limits(w http.ResponseWriter, r *http.Request) {
	ls, err := api(r).Limits(r.Context())
	if err != nil {
		s.apiError(w, err)
		return
	}
	if ls == nil {
		ls = []control.AgentLimits{}
	}
	writeJSON(w, ls)
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := control.ThreadID{Scope: q.Get("scope"), Key: q.Get("key")}
	if id.Scope == "" || id.Key == "" {
		http.Error(w, "scope and key are required", http.StatusBadRequest)
		return
	}
	st, err := api(r).Workspace(r.Context(), id)
	switch {
	case errors.Is(err, control.ErrForbidden) || errors.Is(err, errNotFound):
		// The same answer for both, so a thread outside the caller's
		// scopes cannot be told from no thread.
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	case err != nil:
		s.Log.Warn("dashboard workspace", "scope", id.Scope, "key", id.Key, "err", err)
		http.Error(w, "the workspace cannot be read", http.StatusInternalServerError)
	default:
		writeJSON(w, st)
	}
}

// apiError answers err with its status: 403 for a call the caller may not
// make, 404 for no such thread.
func (s *Server) apiError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, control.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, errNotFound):
		code = http.StatusNotFound
	default:
		s.Log.Warn("dashboard call failed", "err", err)
	}
	http.Error(w, http.StatusText(code), code)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) threads(w http.ResponseWriter, r *http.Request) {
	ts, err := api(r).Threads(r.Context(), control.Filter{})
	if err != nil {
		s.apiError(w, err)
		return
	}
	if ts == nil {
		ts = []control.ThreadInfo{}
	}
	writeJSON(w, ts)
}

// count reads a non-negative integer query parameter.
func count(r *http.Request, name string, def, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: not a count", name)
	}
	if max > 0 && n > max {
		n = max
	}
	return n, nil
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := control.ThreadID{Scope: q.Get("scope"), Key: q.Get("key")}
	if id.Scope == "" || id.Key == "" {
		http.Error(w, "scope and key are required", http.StatusBadRequest)
		return
	}
	recent, err := count(r, "recent", defaultRecent, maxRecent)
	if err == nil {
		var events int
		if events, err = count(r, "events", defaultEvents, control.MaxEvents); err == nil {
			var d control.ThreadDetail
			if d, err = api(r).Thread(r.Context(), id, recent, events); err != nil {
				s.apiError(w, err)
				return
			}
			writeJSON(w, d)
			return
		}
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// position is where a stream starts: after the Last-Event-ID a reconnecting
// page sends, else after the "after" parameter, else from the start.
func position(r *http.Request) (uint64, error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("after")
	}
	if v == "" {
		return 0, nil
	}
	return strconv.ParseUint(v, 10, 64)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, err := position(r)
	if err != nil {
		http.Error(w, "not a position", http.StatusBadRequest)
		return
	}
	stream(s, w, r, func(ctx context.Context) (<-chan control.Event, error) {
		return api(r).Events(ctx, control.Filter{}, after)
	}, s.apiError, func(e control.Event) message {
		m := message{id: strconv.FormatUint(e.Pos, 10), data: e}
		if e.Kind == control.CaughtUp {
			m.event = "caught_up"
		}
		return m
	})
}

// message is one Server-Sent Events message.
type message struct {
	id, event string
	data      any // sent as JSON
}

// stream answers r with a Server-Sent Events stream of what open delivers,
// one message each, until the page goes, the channel closes or the
// session expires: the page must then sign in again. A write that takes
// longer than writeTimeout ends the stream, so that a page that stops
// reading lets go of the follower behind it. When open fails, fail answers.
func stream[T any](s *Server, w http.ResponseWriter, r *http.Request, open func(context.Context) (<-chan T, error),
	fail func(http.ResponseWriter, error), msg func(T) message) {
	exp := sessionExpiry(r)
	ctx, cancel := context.WithDeadline(r.Context(), exp)
	defer cancel()
	ch, err := open(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no") // a proxy must pass each message on at once
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(s.writeBy(exp))
	fmt.Fprint(w, "retry: 1000\n\n")
	rc.Flush()
	tick := time.NewTicker(s.keepAlive)
	defer tick.Stop()
	for {
		rc.SetWriteDeadline(time.Time{})
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			rc.SetWriteDeadline(s.writeBy(exp))
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
		case v, ok := <-ch:
			if !ok {
				return
			}
			m := msg(v)
			b, err := json.Marshal(m.data)
			if err != nil {
				s.Log.Warn("dashboard stream", "id", m.id, "err", err)
				return
			}
			rc.SetWriteDeadline(s.writeBy(exp))
			if m.event != "" {
				fmt.Fprintf(w, "event: %s\n", m.event)
			}
			if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", m.id, b); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
