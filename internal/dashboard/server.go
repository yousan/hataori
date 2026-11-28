// Package dashboard is the web dashboard's HTTP server (design, section
// 7.2). It runs inside the daemon as a client of the control API with a
// Caller that reads, and that may type into a host tmux pane for the owner
// when host input is on (operate.go); it reads nothing else.
//
// It serves on up to two listeners, one per way of reaching it: the
// loopback listener, for a browser on the host and for one on another
// machine through a port forwarded over SSH, and the web listener, over
// HTTPS with a stable name. Which way a request came is the listener that
// accepted its connection, never anything the request says. A sign-in link,
// and the session it opens, belong to one listener.
//
// Every request goes through the same checks, in this order: the connection
// comes from the named reverse proxy, when one is named; the Host is the
// listener's (any loopback name and port on the loopback listener, the
// configured address on the web listener); an Origin, when sent, is exactly
// the listener's own; a request on the loopback listener did not come
// through a reverse proxy; the method is GET or HEAD, except on the
// passkey routes and the operate check, whose other requests must carry the
// listener's exact Origin (changeRoute); only the operate route accepts an
// Upgrade, and its handshake must carry that Origin too; and, for every
// route but the sign-in routes (the trade of a sign-in link and the passkey
// sign-in), the operate WebSocket, whose session is its first message
// (operate.go), and the page's own files, which hold no data, the request
// carries a session of a person who may still sign in, in its
// Authorization header. A session is made by a
// single-use, short-lived link that a person gets with SignInLink, or on
// the web listener by a passkey (passkeys.go), and belongs to that person:
// its requests are made with the control API bound to that person's
// Caller. The link's token is in its fragment: the page trades it for the
// session (SessionPath), so fetching the link uses nothing up.
//
// There is no session cookie. A browser sends a cookie to every port of the
// host that set it, so a service someone else runs on another port of the
// same machine would receive it. The page keeps its session in its own
// origin's storage, which browsers keep apart by scheme, host and port, and
// sends it with each request.
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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/yousan/hataori/internal/control"
)

// DefaultListen is the loopback listener's address when the config names
// none.
const DefaultListen = "127.0.0.1:7466"

// SignInPath is the page a sign-in link opens. The link's token is in the
// address's fragment, which no server and no preview crawler sees: the page
// trades it for a session at SessionPath, so a chat app that fetches the
// link for a preview uses nothing up.
const SignInPath = "/signin"

// SessionPath trades a sign-in link's token, sent as "Authorization: Link
// <token>", for a session: {"session": "<token>"}. The link is used up.
const SessionPath = "/api/session"

// The lifetimes of what the server makes, when the config names none.
const (
	DefaultLinkTTL = 5 * time.Minute
	// DefaultLocalSessionTTL: on the loopback listener the person reached
	// the host by SSH as the instance's user, or is on the host, which is
	// already the proof; signing in again twice a day would be a chore.
	DefaultLocalSessionTTL = 7 * 24 * time.Hour
	DefaultWebSessionTTL   = 12 * time.Hour
)

// Config is how one instance serves its dashboard. At least one of Local,
// Web and Native is set.
type Config struct {
	Local  *Local  // the loopback listener; nil for none
	Web    *Web    // the web listener; nil for none
	Native *Native // private owner transport, independent of browser sign-in

	LinkTTL         time.Duration // how long a sign-in link works; DefaultLinkTTL when zero
	LocalSessionTTL time.Duration // how long a session of the loopback listener lasts; DefaultLocalSessionTTL when zero
	WebSessionTTL   time.Duration // how long a session of the web listener lasts; DefaultWebSessionTTL when zero
	// SessionKey signs sessions, so that a session outlives a restart of the
	// daemon. At least 32 bytes; when empty a random key is made, and
	// sessions end with the process.
	SessionKey []byte
	// Root is the instance root, where the passkeys are kept (PasskeysFile);
	// nil keeps none.
	Root *os.Root
	// Now is the clock; time.Now when nil. Tests set their own.
	Now func() time.Time
	// Bind opens a listener; net.Listen when nil. Tests set their own.
	Bind func(network, address string) (net.Listener, error)
	// HostInput is whether the instance's config turns host input on
	// ("host_input"): without it the operate routes refuse at once. Who may
	// operate a pane is still the core's decision.
	HostInput bool
	// MayOperate reports whether person's sessions may type into a host
	// tmux pane, so that the page shows the switch only to them
	// (OperateMayPath); the daemon answers from the same rule that gives
	// their Caller OpHostInput. Nil: nobody.
	MayOperate func(person string) bool
	// MayAct reports whether person's sessions may give a thread input
	// (act.go), so that the page shows the box, the buttons and the Stop
	// button only to them (ActMayPath); the daemon answers from the same
	// rule that gives their Caller OpSend. Nil: nobody.
	MayAct func(person string) bool
	// AttachHost attaches to the terminal of the session host listening on
	// the socket that AttachTerminal names, as `hataori attach` does
	// (attach.go). Nil: no terminal is attached from the page.
	AttachHost func(socket string) (TerminalInput, error)
	// AttachIdle ends an attachment after this long without a key; the
	// config's host_input_idle. DefaultAttachIdle when zero.
	AttachIdle time.Duration
}

// Local is the loopback listener: plain HTTP on a loopback address.
type Local struct {
	// Listen is the address; DefaultListen when empty. It must be loopback.
	Listen string
	// URL is where its sign-in links point, an http address on a loopback
	// name; made from Listen when empty. Only a config in the old form,
	// one listener, names it.
	URL string
	// Key is the config key of Listen and URL without its last part, for
	// messages: "dashboard." in both forms.
	Key string
}

// Web is the web listener, served over HTTPS by TLS of its own or by a
// reverse proxy in front.
type Web struct {
	// Listen is the TCP address to listen on; DefaultListen when empty (the
	// old form only). An address that is not loopback needs TLS, also
	// behind a proxy.
	Listen string
	// URL is the address people open, such as "https://dash.example.com".
	// Sign-in links start with it, and a request whose Host or Origin is
	// another address is refused. When empty it is made from Listen.
	URL string
	// TLS is the certificate to serve with; nil serves plain HTTP, which is
	// allowed only on loopback behind a proxy.
	TLS *tls.Certificate
	// Proxy is the IP address of a reverse proxy in front that serves the
	// dashboard over TLS at URL. A connection from any other address is
	// refused. Without TLS here, the proxy must be on this machine: Listen
	// is then loopback.
	Proxy string
	// Key is the prefix of its config keys, for messages: "dashboard.web."
	// or, for a config in the old form, "dashboard.".
	Key string
}

// Server is one instance's dashboard.
type Server struct {
	// For returns the control API that a signed-in person's requests are
	// made with, bound to a read-only Caller of that person. ok is false
	// when the person may no longer sign in; their session then answers as
	// none.
	For func(person string) (api control.API, ok bool)
	Log *slog.Logger

	local, web, native *listener
	linkTTL            time.Duration
	now                func() time.Time
	mux                *http.ServeMux

	key []byte // signs sessions
	// ceremonyKey signs passkey sign-in ceremonies. It is made at each
	// start, so that a ceremony answered before a restart, whose answer
	// was recorded only in memory, cannot be answered again after it.
	ceremonyKey []byte

	// Stream timings; tests shorten them.
	keepAlive, writeTimeout, hostScreenInterval, operatedScreenInterval time.Duration

	hostInput  bool                     // the config's host_input
	mayOperate func(person string) bool // Config.MayOperate
	mayAct     func(person string) bool // Config.MayAct
	attachHost func(socket string) (TerminalInput, error)
	attachIdle time.Duration
	// closing is closed when the server closes: the connections the
	// WebSocket routes took over are not the http.Server's to close.
	closing        chan struct{}
	closeOnce      sync.Once
	requestMu      sync.Mutex
	requests       sync.WaitGroup
	stopping       bool
	requestContext context.Context
	cancelRequests context.CancelFunc

	// Passkeys: the web listener's relying party, nil when there is none
	// (rpOff says why), and where they are kept, nil for nowhere.
	rp    *webauthn.WebAuthn
	rpOff string
	keys  *passkeyFile

	mu         sync.Mutex
	links      map[[32]byte]link      // by hash of the token
	grants     map[[32]byte]grant     // proofs for a registration, by hash of the token
	ceremonies map[[32]byte]ceremony  // registrations and checks under way, by hash of their id
	used       map[[32]byte]time.Time // sign-in challenges answered, by hash, until they expire
	operated   map[string]int         // panes being operated through this server, by id
	// operatedChanged is closed when operated changes (operatedChange).
	operatedChanged chan struct{}
	// started is when this server started, and removed when each person
	// last removed a passkey: a session signed in before either may not
	// start an operation on the web listener without a passkey check
	// (operate.go, notRecent).
	started time.Time
	removed map[string]time.Time
	// acted is when each session of the web listener, by its hash, last
	// made a passkey check to act (act.go).
	acted map[[32]byte]time.Time
}

// listener is one way of reaching the dashboard.
type listener struct {
	way    control.Way
	ln     net.Listener
	base   *url.URL // where its links point, canonical: scheme and host, no default port
	proxy  net.IP   // the web listener's reverse proxy, if any
	ttl    time.Duration
	srv    *http.Server
	native *Native // non-nil only for a kernel-checked native Unix listener
}

// link is a sign-in link not used yet: whose session it opens, on which
// listener, and when it expires.
type link struct {
	person string
	way    control.Way
	exp    time.Time
}

// configError is a config error naming its key.
func configError(key, format string, a ...any) error {
	return fmt.Errorf("config: key %q: %s", key, fmt.Sprintf(format, a...))
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

// Listen checks cfg and starts listening on each listener it names. It
// refuses a loopback listener on an address that is not loopback, and a web
// listener that would not be served over HTTPS, and the error says so.
func Listen(cfg Config, api func(person string) (control.API, bool), log *slog.Logger) (*Server, error) {
	if cfg.Local == nil && cfg.Web == nil && cfg.Native == nil {
		return nil, errors.New("dashboard: neither a loopback nor a web listener")
	}
	s := &Server{For: api, Log: log, linkTTL: cfg.LinkTTL, now: cfg.Now,
		links: map[[32]byte]link{}, grants: map[[32]byte]grant{}, ceremonies: map[[32]byte]ceremony{}, used: map[[32]byte]time.Time{}, key: cfg.SessionKey,
		keepAlive: defaultKeepAlive, writeTimeout: defaultWriteTimeout, hostScreenInterval: defaultHostScreenInterval,
		operatedScreenInterval: defaultOperatedScreenInterval, hostInput: cfg.HostInput, mayOperate: cfg.MayOperate, mayAct: cfg.MayAct,
		acted: map[[32]byte]time.Time{}, attachHost: cfg.AttachHost, attachIdle: orDefault(cfg.AttachIdle, DefaultAttachIdle), closing: make(chan struct{})}
	s.requestContext, s.cancelRequests = context.WithCancel(context.Background())
	if len(s.key) == 0 {
		s.key = make([]byte, 32)
		if _, err := rand.Read(s.key); err != nil {
			return nil, fmt.Errorf("dashboard: %w", err)
		}
	} else if len(s.key) < 32 {
		return nil, errors.New("dashboard: the session key is shorter than 32 bytes")
	}
	if s.linkTTL <= 0 {
		s.linkTTL = DefaultLinkTTL
	}
	s.ceremonyKey = make([]byte, 32)
	if _, err := rand.Read(s.ceremonyKey); err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.started, s.removed = s.now(), map[string]time.Time{}
	if cfg.Root != nil {
		s.keys = &passkeyFile{root: cfg.Root, log: log}
		if _, err := s.keys.load(); err != nil { // refused now, not at the first sign-in
			return nil, err
		}
	}
	bind := cfg.Bind
	if bind == nil {
		bind = net.Listen
	}
	var err error
	if cfg.Local != nil {
		if s.local, err = listenLocal(*cfg.Local, bind, orDefault(cfg.LocalSessionTTL, DefaultLocalSessionTTL)); err != nil {
			return nil, err
		}
	}
	if cfg.Web != nil {
		if s.web, err = listenWeb(*cfg.Web, bind, orDefault(cfg.WebSessionTTL, DefaultWebSessionTTL)); err != nil {
			if s.local != nil {
				s.local.ln.Close()
			}
			return nil, err
		}
	}
	if cfg.Native != nil {
		if s.native, err = listenNative(*cfg.Native); err != nil {
			s.Close()
			return nil, err
		}
	}
	s.rpOff = "passkeys are for the web listener, and this dashboard has none"
	if s.web != nil {
		if s.rp, err = relyingParty(s.web); err != nil {
			s.rpOff = err.Error()
			log.Info("dashboard: " + s.rpOff)
		}
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET "+SessionPath, s.signIn)
	s.mux.HandleFunc("GET "+NativeIdentityPath, s.nativeIdentity)
	s.passkeyRoutes()
	s.operateRoutes()
	s.actRoutes()
	s.attachRoutes()
	s.pageRoutes()
	s.routes()
	for _, l := range s.listeners() {
		l.srv = &http.Server{Handler: s.handler(l), ReadHeaderTimeout: 10 * time.Second,
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	}
	return s, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// listenLocal checks and opens the loopback listener.
func listenLocal(c Local, bind func(network, address string) (net.Listener, error), ttl time.Duration) (*listener, error) {
	if c.Key == "" {
		c.Key = "dashboard."
	}
	listen := c.Listen
	if listen == "" {
		listen = DefaultListen
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, configError(c.Key+"listen", "%q is not a host and port such as %q", listen, DefaultListen)
	}
	if !isLoopback(host) {
		return nil, configError(c.Key+"listen", "%q is not a loopback address: the loopback listener serves this machine and ports forwarded to it over SSH; an address others reach is the web listener (%q), served over HTTPS",
			listen, "dashboard.web")
	}
	var base *url.URL
	if c.URL != "" {
		if base, err = parseBase(c.Key+"url", c.URL); err != nil {
			return nil, err
		}
		if base.Scheme != "http" || !loopbackName(base.Host) {
			return nil, configError(c.Key+"url", "%q is not an http address on a loopback name, and the dashboard is served neither with TLS nor behind a reverse proxy", c.URL)
		}
	}
	ln, err := bind("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("dashboard: listen: %w", err)
	}
	// A name such as localhost is what the system resolves it to: make sure
	// that is loopback.
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
		ln.Close()
		return nil, configError(c.Key+"listen", "%q listens on %s, which is not a loopback address", listen, ln.Addr())
	}
	if base == nil {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		base = canonical(&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)})
	}
	return &listener{way: control.WayLocal, ln: ln, base: base, ttl: ttl}, nil
}

// listenWeb checks and opens the web listener: today's rules for a
// dashboard on an address others reach.
func listenWeb(c Web, bind func(network, address string) (net.Listener, error), ttl time.Duration) (*listener, error) {
	if c.Key == "" {
		c.Key = "dashboard.web."
	}
	listen := c.Listen
	if listen == "" {
		listen = DefaultListen
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, configError(c.Key+"listen", "%q is not a host and port such as %q", listen, DefaultListen)
	}
	l := &listener{way: control.WayWeb, ttl: ttl}
	if c.Proxy != "" {
		if l.proxy = net.ParseIP(c.Proxy); l.proxy == nil {
			return nil, configError(c.Key+"proxy", "%q is not an IP address: name the address the reverse proxy connects from", c.Proxy)
		}
	}
	if c.TLS == nil && c.Proxy == "" {
		return nil, configError(c.Key+"listen", "the web listener on %q is served neither with TLS (%q and %q) nor behind a reverse proxy (%q): it would send sign-in links and sessions in the clear",
			listen, c.Key+"tls_cert", c.Key+"tls_key", c.Key+"proxy")
	}
	// A reverse proxy speaking plain HTTP to the dashboard is allowed only on
	// this machine: on any other hop the links and sessions would cross the
	// network in the clear, so a proxy elsewhere needs the dashboard's TLS too.
	if !isLoopback(host) && c.TLS == nil {
		return nil, configError(c.Key+"listen", "%q is not a loopback address: a reverse proxy (%q) on another machine would get sign-in links and sessions in the clear; listen on loopback with the proxy on this machine, or serve with TLS (%q and %q)",
			listen, c.Key+"proxy", c.Key+"tls_cert", c.Key+"tls_key")
	}
	if c.URL != "" {
		if l.base, err = parseBase(c.Key+"url", c.URL); err != nil {
			return nil, err
		}
		if l.base.Scheme != "https" {
			return nil, configError(c.Key+"url", "%q is not https: the web listener is served over HTTPS", c.URL)
		}
	} else if c.Proxy != "" {
		return nil, configError(c.Key+"url", "missing: name the https address the reverse proxy serves the dashboard at")
	} else if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return nil, configError(c.Key+"url", "missing: %q listens on every address, so name the one people open", listen)
	}
	ln, err := bind("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("dashboard: listen: %w", err)
	}
	if l.base == nil {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		l.base = canonical(&url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)})
	}
	if c.TLS != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*c.TLS}, MinVersion: tls.VersionTLS12})
	}
	l.ln = ln
	return l, nil
}

// parseBase parses the URL people open: a scheme and a host, nothing else.
func parseBase(key, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, configError(key, "%q is not an address such as %q", raw, "https://dash.example.com")
	}
	return canonical(u), nil
}

// canonical is u's scheme and host, lower case, without a default port.
func canonical(u *url.URL) *url.URL {
	return &url.URL{Scheme: strings.ToLower(u.Scheme), Host: canonicalHost(u.Scheme, u.Host)}
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

// loopbackName reports whether host, from a Host header or an address, is
// one of the names of this machine's loopback the loopback listener
// answers: localhost, 127.0.0.1 or [::1], with any port or none. Another
// name, even one that resolves to loopback, is not: a page under that name
// would be another site.
func loopbackName(host string) bool {
	h := strings.ToLower(host)
	if hh, port, err := net.SplitHostPort(h); err == nil {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 || strconv.FormatUint(n, 10) != port {
			return false
		}
		h = hh
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// listeners is the listeners the server has, loopback first.
func (s *Server) listeners() []*listener {
	var ls []*listener
	for _, l := range []*listener{s.local, s.web, s.native} {
		if l != nil {
			ls = append(ls, l)
		}
	}
	return ls
}

// listenerOf returns the listener of way; empty way is the loopback
// listener when there is one, else the web listener.
func (s *Server) listenerOf(way control.Way) (*listener, error) {
	switch way {
	case "":
		if s.local != nil {
			return s.local, nil
		}
		return s.web, nil
	case control.WayLocal:
		if s.local == nil {
			return nil, errors.New(`the dashboard has no loopback listener ("dashboard.listen"); a link for its web listener is asked for with the web way (hataori signin --web)`)
		}
		return s.local, nil
	case control.WayWeb:
		if s.web == nil {
			return nil, errors.New(`the dashboard has no web listener ("dashboard.web")`)
		}
		return s.web, nil
	}
	return nil, fmt.Errorf("dashboard: %q is not a listener", way)
}

// URL returns the address of the listener of way that sign-in links start
// with, without a trailing slash, or "" when there is no such listener.
func (s *Server) URL(way control.Way) string {
	l, err := s.listenerOf(way)
	if err != nil || l == nil {
		return ""
	}
	return l.base.String()
}

// Handle adds a route. It is served only after every check of ServeHTTP,
// and only to a signed-in session.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Serve serves every listener until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	ls := s.listeners()
	errs := make(chan error, len(ls))
	for _, l := range ls {
		go func() {
			err := l.srv.Serve(l.ln)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errs <- err
		}()
	}
	var first error
	for range ls {
		if err := <-errs; err != nil && first == nil {
			first = err
			s.Close() // one listener failing stops the dashboard, as before
		}
	}
	return first
}

// Close stops the server and closes its connections and its listeners,
// also when it never served.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.requestMu.Lock()
		s.stopping = true
		close(s.closing)
		s.cancelRequests()
		s.requestMu.Unlock()
	})
	var first error
	for _, l := range s.listeners() {
		var err error
		if l.srv != nil {
			err = l.srv.Close() // closes the listener only once Serve has it
		}
		if e := l.ln.Close(); err == nil && !errors.Is(e, net.ErrClosed) {
			err = e
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// CloseAndWait stops request admission, cancels active streams/upgrades and
// waits for their handlers before the daemon closes their backing store.
func (s *Server) CloseAndWait() error {
	err := s.Close()
	s.requests.Wait()
	return err
}

// NewLink makes a sign-in link for the listener way names (empty: the
// loopback listener when there is one, else the web listener) that works
// once, within the link lifetime, and opens a session of person on that
// listener. The token is kept only as its hash, and is not logged.
func (s *Server) NewLink(_ context.Context, person string, way control.Way) (string, error) {
	if person == "" {
		return "", errors.New("dashboard: a sign-in link needs a person")
	}
	l, err := s.listenerOf(way)
	if err != nil {
		return "", err
	}
	if l == nil {
		return "", errors.New("dashboard: no browser sign-in listener")
	}
	token, sum, err := secret()
	if err != nil {
		return "", err
	}
	now := s.now()
	s.mu.Lock()
	sweep(s.links, now)
	s.links[sum] = link{person: person, way: l.way, exp: now.Add(s.linkTTL)}
	s.mu.Unlock()
	return l.base.String() + SignInPath + "#t=" + token, nil
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

func sweep(m map[[32]byte]link, now time.Time) {
	for k, l := range m {
		if !now.Before(l.exp) {
			delete(m, k)
		}
	}
}

// take removes token from m and returns the person of its session; ok is
// false when it was not there, had expired, or belongs to another listener
// than way. A link presented on the wrong listener is used up too.
func (s *Server) take(m map[[32]byte]link, token string, way control.Way) (person string, ok bool) {
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := m[sum]
	delete(m, sum)
	return l.person, ok && l.way == way && s.now().Before(l.exp)
}

// A session is a token the server signed: its expiry, a random nonce,
// when a passkey was checked for it (zero for none: a session of a link),
// and the person it belongs to, and an HMAC of them and of its listener
// under the session key. The server keeps no list of sessions, so one
// outlives a restart of the daemon that keeps its key. The label names the
// payload's layout and what is signed: a session of an earlier layout no
// longer verifies, and a session of one listener does not verify on the
// other.
const sessionLabel = "hataori dashboard session 4\x00"

// sessionHead is the length of a session's payload before the person: the
// expiry, the nonce and the passkey check, which starts at sessionChecked.
const (
	sessionHead    = 32
	sessionChecked = 24
)

// newSession returns a session of person on the listener way that expires
// at exp; checked is when a passkey was checked for it, zero for none.
func (s *Server) newSession(exp time.Time, person string, way control.Way, checked time.Time) (string, error) {
	payload := make([]byte, sessionHead, sessionHead+len(person))
	binary.BigEndian.PutUint64(payload[:8], uint64(exp.Unix()))
	if _, err := rand.Read(payload[8:sessionChecked]); err != nil {
		return "", fmt.Errorf("dashboard: %w", err)
	}
	if !checked.IsZero() {
		binary.BigEndian.PutUint64(payload[sessionChecked:sessionHead], uint64(checked.Unix()))
	}
	payload = append(payload, person...)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.sign(way, payload)), nil
}

func (s *Server) sign(way control.Way, payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(sessionLabel))
	m.Write([]byte(way))
	m.Write([]byte{0})
	m.Write(payload)
	return m.Sum(nil)
}

// claims is what a session says: when it expires, when a passkey was
// checked for it (zero for none), and whose it is.
type claims struct {
	exp, checked time.Time
	person       string
}

// valid reports whether token is a session of listener l that this key
// signed and that has not expired, and what it says. A session that would
// last longer than l's sessions last now, such as one made before its
// lifetime was shortened, is refused.
func (s *Server) valid(token string, l *listener) (claims, bool) {
	if l.native != nil {
		return claims{}, false
	}
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return claims{}, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || len(payload) <= sessionHead || !hmac.Equal(mac, s.sign(l.way, payload)) {
		return claims{}, false
	}
	c := claims{exp: time.Unix(int64(binary.BigEndian.Uint64(payload[:8])), 0), person: string(payload[sessionHead:])}
	if at := binary.BigEndian.Uint64(payload[sessionChecked:sessionHead]); at != 0 {
		c.checked = time.Unix(int64(at), 0)
	}
	now := s.now()
	return c, now.Before(c.exp) && !c.exp.After(now.Add(l.ttl+time.Minute))
}

// bearer returns the session a request carries in its Authorization header.
func bearer(r *http.Request) (string, bool) { return authorization(r, "Bearer") }

// authorization returns the credential a request carries in its
// Authorization header under scheme.
func authorization(r *http.Request, scheme string) (string, bool) {
	sch, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(sch, scheme) || token == "" {
		return "", false
	}
	return token, true
}

// proxyHeaders are what a reverse proxy adds to a request it forwards. The
// loopback listener refuses them: it serves this machine and ports
// forwarded to it over SSH, which add none, and a proxy that rewrote the
// Host to a loopback name would otherwise open it to whoever the proxy
// serves.
var proxyHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via"}

type sessionKey struct{}
type listenerKey struct{}

// session is what ServeHTTP learned of a request's signed-in session:
// what it says, its hash, which a registration's proof is bound to, and
// the control API of its person.
type session struct {
	claims
	sum [32]byte
	api control.API
}

// sessionOf is the session of a request that passed ServeHTTP's checks.
func sessionOf(r *http.Request) session {
	ss, _ := r.Context().Value(sessionKey{}).(session)
	return ss
}

// sessionExpiry is when the session of a request that passed ServeHTTP's
// checks expires.
func sessionExpiry(r *http.Request) time.Time { return sessionOf(r).exp }

// api is the control API of the person whose session made a request that
// passed ServeHTTP's checks.
func api(r *http.Request) control.API { return sessionOf(r).api }

// listenerOfRequest is the listener that accepted r.
func listenerOfRequest(r *http.Request) *listener {
	l, _ := r.Context().Value(listenerKey{}).(*listener)
	return l
}

// hostOK reports whether host, a request's Host, is one this listener
// answers.
func (l *listener) hostOK(host string) bool {
	if l.native != nil {
		return host == "instance"
	}
	if l.way == control.WayLocal {
		return loopbackName(host)
	}
	return canonicalHost(l.base.Scheme, host) == l.base.Host
}

// originOK reports whether origin, a request's Origin, is exactly this
// listener's own for a request whose Host is host: the scheme, the host and
// the port.
func (l *listener) originOK(origin, host string) bool {
	if l.native != nil {
		return host == "instance" && origin == NativeOrigin
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	want := l.base
	if l.way == control.WayLocal {
		want = canonical(&url.URL{Scheme: "http", Host: host})
	}
	return *canonical(u) == *want
}

// isPageFile reports whether path is one of the page's own files, which
// hold no data and are served without a session: the page reads its
// session from its own storage once it runs. The sign-in page is the page
// itself.
func isPageFile(path string) bool {
	return path == "/" || path == SignInPath || strings.HasPrefix(path, "/assets/")
}

// handler checks every request that l accepted, then hands it to its route.
func (s *Server) handler(l *listener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestMu.Lock()
		if s.stopping {
			s.requestMu.Unlock()
			http.Error(w, "dashboard is stopping", http.StatusServiceUnavailable)
			return
		}
		s.requests.Add(1)
		s.requestMu.Unlock()
		defer s.requests.Done()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(s.requestContext, cancel)
		defer func() { stop(); cancel() }()
		r = r.WithContext(ctx)
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)

		if l.proxy != nil {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			if remote := net.ParseIP(ip); remote == nil || !remote.Equal(l.proxy) {
				s.refuse(w, r, l, http.StatusForbidden, "not from the reverse proxy")
				return
			}
		}
		if !l.hostOK(r.Host) {
			s.refuse(w, r, l, http.StatusForbidden, "unknown host")
			return
		}
		if l.way == control.WayLocal || l.native != nil {
			for _, name := range proxyHeaders {
				if r.Header.Get(name) != "" {
					s.refuse(w, r, l, http.StatusForbidden, "through a reverse proxy: the loopback listener serves this machine and ports forwarded over SSH; put a proxy in front of the web listener")
					return
				}
			}
		}
		if o := r.Header.Get("Origin"); o != "" && !l.originOK(o, r.Host) {
			s.refuse(w, r, l, http.StatusForbidden, "foreign origin")
			return
		}
		if l.native != nil && !nativeRequestOK(r) {
			s.refuse(w, r, l, http.StatusForbidden, "native request has a forbidden route or authority")
			return
		}
		// Only the operate route accepts an Upgrade; the terminal routes
		// refuse one themselves, and log it (dropInput).
		if up := r.Header.Get("Upgrade"); up != "" && r.URL.Path != OperatePath && r.URL.Path != AttachPath && r.URL.Path != TerminalPath && r.URL.Path != HostScreenPath {
			s.refuse(w, r, l, http.StatusBadRequest, "only the operate and attach routes take an upgrade")
			return
		}
		if r.URL.Path == OperatePath || r.URL.Path == AttachPath {
			// A WebSocket's handshake is a GET, sent by the browser with the
			// page's Origin; one without is not from the page.
			if o := r.Header.Get("Origin"); o == "" || o == "null" || !l.originOK(o, r.Host) {
				s.refuse(w, r, l, http.StatusForbidden, "a WebSocket of the page must carry this listener's origin")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !changeRoute(r.Method, r.URL.Path) {
				h.Set("Allow", "GET, HEAD")
				s.refuse(w, r, l, http.StatusMethodNotAllowed, "read-only")
				return
			}
			// A browser sends its page's Origin with every such request; one
			// without is not from the page.
			if o := r.Header.Get("Origin"); o == "" || o == "null" || !l.originOK(o, r.Host) {
				s.refuse(w, r, l, http.StatusForbidden, "a request that changes something must carry this listener's origin")
				return
			}
		}
		ctx = context.WithValue(r.Context(), listenerKey{}, l)
		// The operate WebSocket carries its session in its first message:
		// a browser sends no header with a WebSocket (operate.go).
		if !signInRoute(r.URL.Path) && !isPageFile(r.URL.Path) && r.URL.Path != OperatePath && r.URL.Path != AttachPath {
			ss, ok := s.sessionFrom(r, l)
			if !ok {
				http.Error(w, "not signed in", http.StatusUnauthorized)
				return
			}
			ctx = context.WithValue(ctx, sessionKey{}, ss)
		}
		s.mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionFrom is the session r carries in its Authorization header, when
// it is one of l's that this key signed, has not expired, and belongs to a
// person who may still sign in.
func (s *Server) sessionFrom(r *http.Request, l *listener) (session, bool) {
	if l.native != nil {
		if r.Header.Get("Authorization") != "" {
			return session{}, false
		}
		return l.nativeSession(), true
	}
	token, ok := bearer(r)
	if !ok {
		return session{}, false
	}
	c, ok := s.valid(token, l)
	if !ok {
		return session{}, false
	}
	a, ok := s.For(c.person)
	if !ok || a == nil {
		return session{}, false
	}
	return session{claims: c, sum: sha256.Sum256([]byte(token)), api: a}, true
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, l *listener, code int, why string) {
	s.Log.Warn("dashboard request refused", "why", why, "listener", string(l.way), "method", r.Method, "host", r.Host, "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
	http.Error(w, why, code)
}

// signIn trades a sign-in link's token for a session of the listener it
// was made for, and answers {"session": "<token>"}, on the web listener
// with the proof that registers a passkey ("register"). The link is used
// up whether or not it was still valid, and whichever listener it was
// shown on. A HEAD uses nothing up.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, "ask with GET", http.StatusMethodNotAllowed)
		return
	}
	l := listenerOfRequest(r)
	link, _ := authorization(r, "Link")
	person, ok := s.take(s.links, link, l.way)
	if !ok {
		http.Error(w, "this sign-in link was used or has expired: ask for a new one", http.StatusUnauthorized)
		return
	}
	token, err := s.newSession(s.now().Add(l.ttl), person, l.way, time.Time{})
	if err != nil {
		http.Error(w, "cannot make a session", http.StatusInternalServerError)
		return
	}
	out := signedIn{Session: token}
	if l.way == control.WayWeb && s.rp != nil && s.keys != nil {
		if out.Register, err = s.newGrant(person, token, nil); err != nil {
			http.Error(w, "cannot make a session", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, out)
}
