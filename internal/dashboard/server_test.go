package dashboard

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// start serves cfg and returns the server and its address.
func start(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	s, err := Listen(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.Handle("GET /events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "stream") }))
	// A route that names no method: only the server's own check keeps it read-only.
	s.Handle("/any", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "any") }))
	s.Handle("GET /terminal/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "stream") }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

// client follows no redirect and sends each request to the server's
// listener, whatever its Host.
func client(s *Server) *http.Client {
	addr := s.ln.Addr().String()
	d := &net.Dialer{}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type req struct {
	method, path, host, origin string
	cookie                     *http.Cookie
}

func do(t *testing.T, s *Server, r req) *http.Response {
	t.Helper()
	if r.method == "" {
		r.method = http.MethodGet
	}
	// To the listener itself, as a reverse proxy would send it: over TLS
	// only when the dashboard serves TLS itself, with the dashboard's own
	// Host unless the test gives another.
	scheme := "http"
	if s.base.Scheme == "https" && s.proxy == nil {
		scheme = "https"
	}
	hr, err := http.NewRequest(r.method, scheme+"://"+s.ln.Addr().String()+r.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	hr.Host = s.base.Host
	if r.host != "" {
		hr.Host = r.host
	}
	if r.origin != "" {
		hr.Header.Set("Origin", r.origin)
	}
	if r.cookie != nil {
		hr.AddCookie(r.cookie)
	}
	resp, err := client(s).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// signIn uses a new link and returns the session cookie.
func signIn(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	link, err := s.NewLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, s, req{path: pathOf(t, link)})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in link: %s", resp.Status)
	}
	for _, c := range resp.Cookies() {
		if c.Name == s.cookie {
			return c
		}
	}
	t.Fatalf("sign-in set no session cookie: %v", resp.Header["Set-Cookie"])
	return nil
}

func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

var routes = []string{"/", "/events", "/terminal/7", "/no-such-route", "/signin/x"}

func TestEveryRouteNeedsASession(t *testing.T) {
	s := start(t, Config{})
	for _, p := range routes {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			if resp := do(t, s, req{method: m, path: p}); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s without a session: %s, want 401", m, p, resp.Status)
			}
			bogus := &http.Cookie{Name: s.cookie, Value: "made-up"}
			if resp := do(t, s, req{method: m, path: p, cookie: bogus}); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with a made-up session: %s, want 401", m, p, resp.Status)
			}
		}
	}
	c := signIn(t, s)
	for _, p := range []string{"/", "/events", "/terminal/7"} {
		if resp := do(t, s, req{path: p, cookie: c}); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s signed in: %s", p, resp.Status)
		}
	}
}

func TestALinkWorksOnce(t *testing.T) {
	s := start(t, Config{})
	link, _ := s.NewLink(context.Background())
	p := pathOf(t, link)
	if resp := do(t, s, req{path: p}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use: %s", resp.Status)
	}
	if resp := do(t, s, req{path: p}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second use: %s, want 401", resp.Status)
	}
	if resp := do(t, s, req{path: "/signin?t=made-up"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a made-up link: %s, want 401", resp.Status)
	}
	if resp := do(t, s, req{path: "/signin"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %s, want 401", resp.Status)
	}
}

func TestAnExpiredLinkIsRefused(t *testing.T) {
	s := start(t, Config{})
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	link, _ := s.NewLink(context.Background())
	mu.Lock()
	now = now.Add(s.linkTTL)
	mu.Unlock()
	if resp := do(t, s, req{path: pathOf(t, link)}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired link: %s, want 401", resp.Status)
	}
}

func TestASessionExpires(t *testing.T) {
	s := start(t, Config{})
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := signIn(t, s)
	mu.Lock()
	now = now.Add(s.sessionTTL)
	mu.Unlock()
	if resp := do(t, s, req{path: "/", cookie: c}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session: %s, want 401", resp.Status)
	}
}

func TestUnknownHostAndForeignOriginAreRefused(t *testing.T) {
	s := start(t, Config{})
	c := signIn(t, s)
	_, port, _ := net.SplitHostPort(s.ln.Addr().String())
	for _, host := range []string{"evil.example.com", "evil.example.com:" + port, "localhost:" + port, "127.0.0.1"} {
		for _, p := range []string{"/", "/signin?t=x"} {
			if resp := do(t, s, req{path: p, host: host, cookie: c}); resp.StatusCode != http.StatusForbidden {
				t.Errorf("Host %s, %s: %s, want 403", host, p, resp.Status)
			}
		}
	}
	for _, origin := range []string{"http://evil.example.com", "https://127.0.0.1:" + port, "http://127.0.0.1:" + port + "/x", "null"} {
		if resp := do(t, s, req{path: "/", origin: origin, cookie: c}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %s: %s, want 403", origin, resp.Status)
		}
	}
	if resp := do(t, s, req{path: "/", origin: s.URL(), cookie: c}); resp.StatusCode != http.StatusOK {
		t.Errorf("own Origin: %s", resp.Status)
	}
}

func TestOnlyReadsAreServed(t *testing.T) {
	s := start(t, Config{})
	c := signIn(t, s)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
		for _, p := range []string{"/", "/events", "/any", "/no-such-route", "/signin?t=x"} {
			if resp := do(t, s, req{method: m, path: p, cookie: c}); resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %s, want 405", m, p, resp.Status)
			}
		}
	}
}

func TestNotLoopbackNeedsTLSOrAProxy(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "[::]:0", "dash.example.com:0"} {
		_, err := Listen(Config{Listen: listen}, nil, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "not a loopback address") || !strings.Contains(err.Error(), "dashboard.proxy") {
			t.Errorf("listen %s: %v", listen, err)
		}
	}
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Listen: "127.0.0.1:0", Proxy: "127.0.0.1"}, `"dashboard.url": missing`},
		{Config{Listen: "127.0.0.1:0", Proxy: "127.0.0.1", URL: "http://dash.example.com"}, "not https"},
		{Config{Listen: "127.0.0.1:0", Proxy: "proxy.example.com", URL: "https://dash.example.com"}, "not an IP address"},
		{Config{Listen: "127.0.0.1:0", URL: "https://dash.example.com"}, "is https"},
		{Config{Listen: "127.0.0.1:0", URL: "https://dash.example.com/path"}, "is not an address"},
		{Config{Listen: "nonsense"}, "is not a host and port"},
	} {
		if _, err := Listen(c.cfg, nil, slog.Default()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.want)
		}
	}
}

func TestCookieOverPlainHTTPIsHttpOnly(t *testing.T) {
	s := start(t, Config{})
	c := signIn(t, s)
	if !c.HttpOnly || c.Secure {
		t.Fatalf("cookie: HttpOnly %v Secure %v, want HttpOnly only", c.HttpOnly, c.Secure)
	}
}

func TestCookieOverTLSIsSecure(t *testing.T) {
	s := start(t, Config{TLS: selfSigned(t)})
	if !strings.HasPrefix(s.URL(), "https://") {
		t.Fatalf("URL %s", s.URL())
	}
	c := signIn(t, s)
	if !c.HttpOnly || !c.Secure || !strings.HasPrefix(c.Name, "__Host-") {
		t.Fatalf("cookie %s: HttpOnly %v Secure %v", c.Name, c.HttpOnly, c.Secure)
	}
	if resp := do(t, s, req{path: "/", cookie: c}); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %s", resp.Status)
	}
}

func TestBehindAProxy(t *testing.T) {
	// The proxy speaks plain HTTP to the dashboard and forwards the Host it
	// was asked for.
	s := start(t, Config{Proxy: "127.0.0.1", URL: "https://Dash.Example.com:443/"})
	if s.URL() != "https://dash.example.com" {
		t.Fatalf("URL %s", s.URL())
	}
	link, _ := s.NewLink(context.Background())
	if !strings.HasPrefix(link, "https://dash.example.com/signin?t=") {
		t.Fatalf("link %s", link)
	}
	resp := do(t, s, req{path: pathOf(t, link), host: "dash.example.com"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %s", resp.Status)
	}
	var c *http.Cookie
	for _, x := range resp.Cookies() {
		c = x
	}
	if c == nil || !c.HttpOnly || !c.Secure {
		t.Fatalf("cookie behind the proxy: %+v", c)
	}
	if resp := do(t, s, req{path: "/", host: "dash.example.com", origin: "https://dash.example.com", cookie: c}); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %s", resp.Status)
	}
	if resp := do(t, s, req{path: "/", host: s.ln.Addr().String(), cookie: c}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the listener's own address as Host: %s, want 403", resp.Status)
	}
}

func TestOnlyTheProxyMayConnect(t *testing.T) {
	s := start(t, Config{Proxy: "192.0.2.1", URL: "https://dash.example.com"})
	if resp := do(t, s, req{path: "/", host: "dash.example.com"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("not from the proxy: %s, want 403", resp.Status)
	}
}

func selfSigned(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
