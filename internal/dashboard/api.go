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
//
// The event stream sends each event as one message whose id is the event's
// position, and the caught_up mark as a message of type caught_up. A page
// that reconnects sends the last id it saw as Last-Event-ID, as a browser's
// EventSource does, and gets every later event once.
const (
	ThreadsPath = "/api/threads"
	ThreadPath  = "/api/thread"
	EventsPath  = "/api/events"
)

// Defaults and caps of /api/thread.
const (
	defaultRecent = 4096
	maxRecent     = 64 << 10
	defaultEvents = 20
)

// keepAlive is how often an idle event stream sends a comment, so that a
// proxy or a browser does not close it.
var keepAlive = 15 * time.Second

// writeTimeout is how long one write to an event stream may take. A page
// that stops reading loses its stream, and the follower behind it ends.
var writeTimeout = 30 * time.Second

func (s *Server) routes() {
	s.mux.HandleFunc("GET "+ThreadsPath, s.threads)
	s.mux.HandleFunc("GET "+ThreadPath, s.thread)
	s.mux.HandleFunc("GET "+EventsPath, s.events)
}

// apiError answers err with its status: 403 for a call the caller may not
// make, 404 for no such thread.
func (s *Server) apiError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, control.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, core.ErrNotFound):
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
	ts, err := s.API.Threads(r.Context(), control.Filter{})
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
			if d, err = s.API.Thread(r.Context(), id, recent, events); err != nil {
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
	// The stream ends when its session does: the page must sign in again.
	ctx, cancel := context.WithDeadline(r.Context(), sessionExpiry(r))
	defer cancel()
	evs, err := s.API.Events(ctx, control.Filter{}, after)
	if err != nil {
		s.apiError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no") // a proxy must pass each event on at once
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	fmt.Fprint(w, "retry: 1000\n\n")
	rc.Flush()
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		rc.SetWriteDeadline(time.Time{})
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
		case e, ok := <-evs:
			if !ok {
				return
			}
			b, err := json.Marshal(e)
			if err != nil {
				s.Log.Warn("dashboard event", "pos", e.Pos, "err", err)
				return
			}
			rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if e.Kind == control.CaughtUp {
				fmt.Fprint(w, "event: caught_up\n")
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Pos, b); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
