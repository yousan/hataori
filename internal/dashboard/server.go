// Package dashboard is the read-only web dashboard's HTTP server (design,
// section 7.2). It runs inside the daemon as a client of the control API
// with a read-only Caller, and reads nothing else.
//
// Every request goes through the same checks, in this order: the connection
// comes from the named reverse proxy, when one is named; the Host is the
// dashboard's address; an Origin, when sent, is the dashboard's own; the
// method is GET or HEAD; and, for every route but the sign-in link itself,
// the request carries a signed-in session. A session is made only by a
// single-use, short-lived link that the owner gets with SignInLink.
package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// DefaultListen is the address the dashboard listens on when the config
// names none: loopback only.
const DefaultListen = "127.0.0.1:7466"

// SignInPath is the route of a sign-in link.
const SignInPath = "/signin"

// Config is how one instance serves its dashboard.
type Config struct {
	// Listen is the TCP address to listen on; DefaultListen when empty. An
	// address that is not loopback needs TLS, also behind a proxy.
	Listen string
	// URL is the address people open, such as "https://dash.example.com".
	// Sign-in links start with it, and a request whose Host or Origin is
	// another address is refused. When empty it is made from Listen.
	URL string
	// TLS is the certificate to serve with; nil serves plain HTTP.
	TLS *tls.Certificate
	// Proxy is the IP address of a reverse proxy in front that serves the
	// dashboard over TLS at URL. A connection from any other address is
	// refused. Without TLS here, the proxy must be on this machine: Listen
	// is then loopback.
	Proxy string

	LinkTTL    time.Duration // how long a sign-in link works; 5 minutes when zero
	SessionTTL time.Duration // how long a session lasts; 12 hours when zero
	// SessionKey signs session cookies, so that a session outlives a
	// restart of the daemon. At least 32 bytes; when empty a random key is
	// made, and sessions end with the process.
	SessionKey []byte
}

// Server is one instance's dashboard.
type Server struct {
	// API is the control API, bound to a read-only Caller.
	API control.API
	Log *slog.Logger

	ln         net.Listener
	base       *url.URL // canonical: scheme and host, no default port
	proxy      net.IP
	secure     bool // over TLS, ours or the proxy's
	cookie     string
	linkTTL    time.Duration
	sessionTTL time.Duration
	now        func() time.Time
	mux        *http.ServeMux
	srv        *http.Server

	key []byte // signs sessions

	mu    sync.Mutex
	links map[[32]byte]time.Time // by hash of the token: when it expires
}

// configError is a config error naming its key.
func configError(key, format string, a ...any) error {
	return fmt.Errorf("config: key %q: %s", "dashboard."+key, fmt.Sprintf(format, a...))
}

// isLoopback reports whether host, from a listening address, reaches only
// this machine.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Listen checks cfg and starts listening. It refuses to listen on an address
// other than loopback unless the dashboard is served over TLS, its own or a
// named reverse proxy's, and the error says so.
func Listen(cfg Config, api control.API, log *slog.Logger) (*Server, error) {
	listen := cfg.Listen
	if listen == "" {
		listen = DefaultListen
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, configError("listen", "%q is not a host and port such as %q", listen, DefaultListen)
	}
	s := &Server{API: api, Log: log, secure: cfg.TLS != nil || cfg.Proxy != "",
		linkTTL: cfg.LinkTTL, sessionTTL: cfg.SessionTTL, now: time.Now,
		links: map[[32]byte]time.Time{}, key: cfg.SessionKey}
	if len(s.key) == 0 {
		s.key = make([]byte, 32)
		if _, err := rand.Read(s.key); err != nil {
			return nil, fmt.Errorf("dashboard: %w", err)
		}
	} else if len(s.key) < 32 {
		return nil, errors.New("dashboard: the session key is shorter than 32 bytes")
	}
	if s.linkTTL <= 0 {
		s.linkTTL = 5 * time.Minute
	}
	if s.sessionTTL <= 0 {
		s.sessionTTL = 12 * time.Hour
	}
	if cfg.Proxy != "" {
		if s.proxy = net.ParseIP(cfg.Proxy); s.proxy == nil {
			return nil, configError("proxy", "%q is not an IP address: name the address the reverse proxy connects from", cfg.Proxy)
		}
	}
	// A reverse proxy speaking plain HTTP to the dashboard is allowed only on
	// this machine: on any other hop the links and sessions would cross the
	// network in the clear, so a proxy elsewhere needs the dashboard's TLS too.
	if !isLoopback(host) && cfg.TLS == nil {
		if cfg.Proxy != "" {
			return nil, configError("listen", "%q is not a loopback address: a reverse proxy (%q) on another machine would get sign-in links and sessions in the clear; listen on loopback with the proxy on this machine, or serve with TLS (%q and %q)",
				listen, "dashboard.proxy", "dashboard.tls_cert", "dashboard.tls_key")
		}
		return nil, configError("listen", "%q is not a loopback address, and the dashboard is served neither with TLS (%q and %q) nor behind a reverse proxy on this machine (%q): it would send sign-in links and sessions in the clear",
			listen, "dashboard.tls_cert", "dashboard.tls_key", "dashboard.proxy")
	}
	if cfg.URL != "" {
		if s.base, err = parseBase(cfg.URL); err != nil {
			return nil, err
		}
		switch {
		case cfg.Proxy != "" && s.base.Scheme != "https":
			return nil, configError("url", "%q is not https: the reverse proxy named in %q must serve the dashboard over TLS", cfg.URL, "dashboard.proxy")
		case cfg.TLS != nil && s.base.Scheme != "https":
			return nil, configError("url", "%q is not https, and the dashboard is served with TLS", cfg.URL)
		case cfg.TLS == nil && cfg.Proxy == "" && s.base.Scheme != "http":
			return nil, configError("url", "%q is https, and the dashboard is served neither with TLS nor behind a reverse proxy", cfg.URL)
		}
	} else if cfg.Proxy != "" {
		return nil, configError("url", "missing: name the https address the reverse proxy serves the dashboard at")
	} else if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return nil, configError("url", "missing: %q listens on every address, so name the one people open", listen)
	}
	if s.secure {
		s.cookie = "__Host-hataori-session"
	} else {
		s.cookie = "hataori-session"
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("dashboard: listen: %w", err)
	}
	if s.base == nil {
		scheme := "http"
		if cfg.TLS != nil {
			scheme = "https"
		}
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		s.base = canonical(&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)})
	}
	if cfg.TLS != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cfg.TLS}, MinVersion: tls.VersionTLS12})
	}
	s.ln = ln
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET "+SignInPath, s.signIn)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Signed in to the hataori dashboard.")
	})
	s.routes()
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	return s, nil
}

// parseBase parses the URL people open: a scheme and a host, nothing else.
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, configError("url", "%q is not an address such as %q", raw, "https://dash.example.com")
	}
	return canonical(u), nil
}

// canonical is u's scheme and host, lower case, without a default port.
func canonical(u *url.URL) *url.URL {
	c := &url.URL{Scheme: strings.ToLower(u.Scheme), Host: canonicalHost(u.Scheme, u.Host)}
	return c
}

func canonicalHost(scheme, host string) string {
	host = strings.ToLower(host)
	if h, port, err := net.SplitHostPort(host); err == nil {
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			if strings.Contains(h, ":") {
				return "[" + h + "]"
			}
			return h
		}
	}
	return host
}

// URL returns the address people open, without a trailing slash.
func (s *Server) URL() string { return s.base.String() }

// Handle adds a route. It is served only after every check of ServeHTTP,
// and only to a signed-in session.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Serve serves until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the server and closes its connections.
func (s *Server) Close() error { return s.srv.Close() }

// NewLink makes a sign-in link that works once, within the link lifetime.
func (s *Server) NewLink(context.Context) (string, error) {
	token, sum, err := secret()
	if err != nil {
		return "", err
	}
	now := s.now()
	s.mu.Lock()
	sweep(s.links, now)
	s.links[sum] = now.Add(s.linkTTL)
	s.mu.Unlock()
	return s.base.String() + SignInPath + "?t=" + token, nil
}

// secret returns a new random token and its hash.
func secret() (string, [32]byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", [32]byte{}, fmt.Errorf("dashboard: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func sweep(m map[[32]byte]time.Time, now time.Time) {
	for k, exp := range m {
		if !now.Before(exp) {
			delete(m, k)
		}
	}
}

// take removes token from m and reports whether it was there and had not
// expired.
func (s *Server) take(m map[[32]byte]time.Time, token string) bool {
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := m[sum]
	delete(m, sum)
	return ok && s.now().Before(exp)
}

// A session is a cookie the server signed: its expiry and a random nonce,
// and an HMAC of them under the session key. The server keeps no list of
// sessions, so one outlives a restart of the daemon that keeps its key.
const sessionLabel = "hataori dashboard session\x00"

// newSession returns a session that expires at exp.
func (s *Server) newSession(exp time.Time) (string, error) {
	var payload [24]byte
	binary.BigEndian.PutUint64(payload[:8], uint64(exp.Unix()))
	if _, err := rand.Read(payload[8:]); err != nil {
		return "", fmt.Errorf("dashboard: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload[:]) + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload[:])), nil
}

func (s *Server) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(sessionLabel))
	m.Write(payload)
	return m.Sum(nil)
}

// valid reports whether token is a session this key signed that has not
// expired, and when it expires.
func (s *Server) valid(token string) (time.Time, bool) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return time.Time{}, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || len(payload) != 24 || !hmac.Equal(mac, s.sign(payload)) {
		return time.Time{}, false
	}
	exp := time.Unix(int64(binary.BigEndian.Uint64(payload[:8])), 0)
	return exp, s.now().Before(exp)
}

type expiryKey struct{}

// sessionExpiry is when the session of a request that passed ServeHTTP's
// checks expires.
func sessionExpiry(r *http.Request) time.Time {
	exp, _ := r.Context().Value(expiryKey{}).(time.Time)
	return exp
}

// ServeHTTP checks every request, then hands it to its route.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")

	if s.proxy != nil {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if remote := net.ParseIP(ip); remote == nil || !remote.Equal(s.proxy) {
			s.refuse(w, r, http.StatusForbidden, "not from the reverse proxy")
			return
		}
	}
	if canonicalHost(s.base.Scheme, r.Host) != s.base.Host {
		s.refuse(w, r, http.StatusForbidden, "unknown host")
		return
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Path != "" || *canonical(u) != *s.base {
			s.refuse(w, r, http.StatusForbidden, "foreign origin")
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		s.refuse(w, r, http.StatusMethodNotAllowed, "read-only")
		return
	}
	if r.URL.Path != SignInPath {
		c, err := r.Cookie(s.cookie)
		if err != nil {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		exp, ok := s.valid(c.Value)
		if !ok {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), expiryKey{}, exp))
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, code int, why string) {
	s.Log.Warn("dashboard request refused", "why", why, "method", r.Method, "host", r.Host, "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
	http.Error(w, why, code)
}

// signIn turns a sign-in link into a session. The link is used up whether
// or not it was still valid.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead { // a link checker must not use the link up
		w.Header().Set("Allow", "GET")
		http.Error(w, "open the link with GET", http.StatusMethodNotAllowed)
		return
	}
	if !s.take(s.links, r.URL.Query().Get("t")) {
		http.Error(w, "this sign-in link was used or has expired: ask for a new one", http.StatusUnauthorized)
		return
	}
	token, err := s.newSession(s.now().Add(s.sessionTTL))
	if err != nil {
		http.Error(w, "cannot make a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: token, Path: "/", MaxAge: int(s.sessionTTL / time.Second),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
