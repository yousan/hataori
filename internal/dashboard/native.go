package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/nativehttp"
)

// NativeOrigin is the app's fixed origin. Its main-process transport opens
// the Unix socket directly; this name is never an owner TCP endpoint.
const NativeOrigin = "hataori-desktop://instance"

// NativeWay labels private HTTP/audit responses, not a control-protocol
// sign-in destination. NewLink and listenerOf never accept it.
const NativeWay control.Way = "native"

const NativeIdentityPath = "/api/native"

// nativeRequestOK rejects HTTP proxy forms, noncanonical routes and
// ambiguous credentials before ServeMux can redirect or select a route.
func nativeRequestOK(r *http.Request) bool {
	if r.URL.IsAbs() || r.URL.RawPath != "" || path.Clean(r.URL.Path) != r.URL.Path || !nativeRoute(r.Method, r.URL.Path) {
		return false
	}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "forwarded" || lower == "via" || lower == "x-real-ip" || strings.HasPrefix(lower, "x-forwarded-") {
			return false
		}
		if lower == "origin" && (len(values) != 1 || values[0] != NativeOrigin) {
			return false
		}
	}
	return true
}

// Native binds an OS-private listener to the daemon's owner dashboard API.
// All fields come from daemon startup, never requests or proxy headers.
type Native struct {
	Transport nativehttp.Config
	Person    string
	API       control.API
}

func listenNative(cfg Native) (*listener, error) {
	if cfg.Person == "" || cfg.API == nil {
		return nil, errors.New("dashboard: native listener needs a daemon-bound owner")
	}
	ln, err := nativehttp.Listen(cfg.Transport)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(NativeOrigin)
	return &listener{way: NativeWay, ln: ln, base: base, native: &cfg}, nil
}

func (l *listener) nativeSession() session {
	// There is no bearer lifetime on this transport. Each request belongs
	// to its checked connection and ends with the server/connection. The
	// far-future deadline lets existing streams retain their common code.
	return session{claims: claims{person: l.native.Person, exp: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)}, api: l.native.API}
}

// websocketSession authenticates the first message on the listener that
// accepted its connection. Native messages have an empty session field;
// browser credentials never cross into this authority path.
func (s *Server) websocketSession(l *listener, token string) (session, bool) {
	if l.native != nil {
		if token != "" {
			return session{}, false
		}
		return l.nativeSession(), true
	}
	c, ok := s.valid(token, l)
	if !ok {
		return session{}, false
	}
	a, ok := s.For(c.person)
	if !ok || a == nil {
		return session{}, false
	}
	return session{claims: c, api: a}, true
}

// nativeRoute is deliberately smaller than the server's mutable mux:
// adding Handle must never expose an arbitrary new owner route here.
func nativeRoute(method, path string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		if path == "/" || strings.HasPrefix(path, "/assets/") {
			return true
		}
		switch path {
		case NativeIdentityPath, ThreadsPath, ScopesPath, LayoutPath, ThreadPath,
			ThreadEventsPath, EventsPath, TerminalPath, HostScreenPath, LimitsPath,
			ClientsPath, TrustsPath, WorkspacePath, HostTerminalsPath, MePath,
			ActMayPath, OperateMayPath, OperatePath, AttachPath:
			return true
		}
	}
	if method == http.MethodPost {
		switch path {
		case ActCheckPath, SendPath, AnswerPath, StopPath, ChoosePath, OperateCheckPath:
			return true
		}
	}
	return false
}

func (s *Server) nativeIdentity(w http.ResponseWriter, r *http.Request) {
	l := listenerOfRequest(r)
	if l.native == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, struct {
		Instance string `json:"instance"`
		Version  string `json:"version"`
		Native   bool   `json:"native"`
	}{l.native.Transport.Instance, l.native.Transport.Version, true})
}
