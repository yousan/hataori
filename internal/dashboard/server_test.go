package dashboard

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// start serves cfg and returns the server. Each tune changes the server
// before it serves.
func start(t *testing.T, cfg Config, tune ...func(*Server)) *Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	s, err := Listen(cfg, only(noAPI{}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.Handle("GET /events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "stream") }))
	// A route that names no method: only the server's own check keeps it read-only.
	s.Handle("/any", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "any") }))
	s.Handle("GET /terminal/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "stream") }))
	for _, f := range tune {
		f(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

// noAPI is a control API whose operations no test calls.
type noAPI struct{ control.API }

// only gives a's sessions to the owner, and none to anyone else.
func only(a control.API) func(string) (control.API, bool) {
	return func(person string) (control.API, bool) { return a, person == "owner" }
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
	link, err := s.NewLink(context.Background(), "owner")
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
	link, _ := s.NewLink(context.Background(), "owner")
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

	// A HEAD, such as a link checker's, does not use a link up.
	link, _ = s.NewLink(context.Background(), "owner")
	p = pathOf(t, link)
	if resp := do(t, s, req{method: http.MethodHead, path: p}); resp.StatusCode != http.StatusMethodNotAllowed || len(resp.Cookies()) != 0 {
		t.Fatalf("HEAD: %s, cookies %v", resp.Status, resp.Cookies())
	}
	if resp := do(t, s, req{path: p}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET after a HEAD: %s", resp.Status)
	}
}

// A session belongs to the person its link was made for: its requests are
// made with that person's API, and it answers as none once that person may
// no longer sign in. A session of the earlier layout, without a person, is
// refused.
func TestASessionBelongsToItsPerson(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	long := strings.Repeat("a", 1000) // the config sets no limit on a name
	allowed := map[string]bool{"ada": true, "grace": true, long: true}
	s := start(t, Config{}, func(s *Server) {
		s.For = func(person string) (control.API, bool) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, person)
			return noAPI{}, allowed[person]
		}
	})
	for _, person := range []string{"ada", "grace", long} {
		link, err := s.NewLink(context.Background(), person)
		if err != nil {
			t.Fatal(err)
		}
		resp := do(t, s, req{path: pathOf(t, link)})
		if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 {
			t.Fatalf("%s: sign-in %s", person, resp.Status)
		}
		c := resp.Cookies()[0]
		mu.Lock()
		asked = nil
		mu.Unlock()
		if resp := do(t, s, req{path: "/", cookie: c}); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s signed in: %s", person, resp.Status)
		}
		mu.Lock()
		if !reflect.DeepEqual(asked, []string{person}) {
			t.Fatalf("%s's request asked for the API of %q", person, asked)
		}
		allowed[person] = false
		mu.Unlock()
		if resp := do(t, s, req{path: "/", cookie: c}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s may no longer sign in: %s, want 401", person, resp.Status)
		}
	}
	if _, err := s.NewLink(context.Background(), ""); err == nil {
		t.Fatal("a link for nobody was made")
	}

	// The earlier layout: expiry and nonce, signed under the earlier label.
	var payload [24]byte
	binary.BigEndian.PutUint64(payload[:8], uint64(time.Now().Add(time.Hour).Unix()))
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("hataori dashboard session\x00"))
	m.Write(payload[:])
	old := base64.RawURLEncoding.EncodeToString(payload[:]) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if resp := do(t, s, req{path: "/", cookie: &http.Cookie{Name: s.cookie, Value: old}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session without a person: %s, want 401", resp.Status)
	}
}

func TestAnExpiredLinkIsRefused(t *testing.T) {
	s := start(t, Config{})
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	link, _ := s.NewLink(context.Background(), "owner")
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
		// A proxy on another machine would get links and sessions in the clear.
		{Config{Listen: "0.0.0.0:0", Proxy: "192.0.2.1", URL: "https://dash.example.com"}, "on another machine"},
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
	link, _ := s.NewLink(context.Background(), "owner")
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

// streamAPI is a control API whose Events sends big events as fast as it
// may until its context ends, and then reports that it ended.
type streamAPI struct {
	control.API
	ended chan struct{}
}

func (a streamAPI) Events(ctx context.Context, _ control.Filter, _ uint64) (<-chan control.Event, error) {
	out := make(chan control.Event)
	data, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 64<<10)})
	go func() {
		defer close(a.ended)
		defer close(out)
		for pos := uint64(1); ; pos++ {
			select {
			case out <- control.Event{Pos: pos, Kind: "text", Data: data}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// openStream opens the event stream and returns its body.
func openStream(t *testing.T, s *Server, c *http.Cookie) io.ReadCloser {
	t.Helper()
	hr, _ := http.NewRequest(http.MethodGet, "http://"+s.ln.Addr().String()+EventsPath, nil)
	hr.Host = s.base.Host
	hr.AddCookie(c)
	resp, err := client(s).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %s", resp.Status)
	}
	return resp.Body
}

// A stream ends when its session expires.
func TestAStreamEndsWithItsSession(t *testing.T) {
	ended := make(chan struct{})
	s := start(t, Config{SessionTTL: 2 * time.Second}, func(s *Server) { s.For = only(streamAPI{ended: ended}) })
	body := openStream(t, s, signIn(t, s))
	defer body.Close()
	go io.Copy(io.Discard, body)
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream outlived its session")
	}
}

// A page that stops reading loses its stream, and the follower behind it
// ends.
func TestAStalledStreamEnds(t *testing.T) {
	ended := make(chan struct{})
	s := start(t, Config{}, func(s *Server) { s.For, s.writeTimeout = only(streamAPI{ended: ended}), 500*time.Millisecond })
	body := openStream(t, s, signIn(t, s))
	defer body.Close() // never read
	select {
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("a stream nobody reads went on")
	}
}

// screenAPI is a control API whose HostScreen answers pane %1 with the
// screen the test sets, %8 as no pane, and every other pane as forbidden.
type screenAPI struct {
	control.API
	mu     sync.Mutex
	screen string
	gone   bool
	reads  int
}

func (a *screenAPI) set(screen string, gone bool) {
	a.mu.Lock()
	a.screen, a.gone = screen, gone
	a.mu.Unlock()
}

func (a *screenAPI) HostScreen(_ context.Context, pane string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	switch {
	case pane == "%1" && !a.gone:
		return a.screen, nil
	case pane == "%1" || pane == "%8":
		return "", fmt.Errorf("core: HostScreen: tmux pane %q: %w", pane, core.ErrNotFound)
	}
	return "", fmt.Errorf("core: HostScreen: %w", control.ErrForbidden)
}

// logTo makes s log into a buffer and returns what it logged so far.
func logTo(s *Server) func() string {
	var mu sync.Mutex
	var b strings.Builder
	s.Log = slog.New(slog.NewTextHandler(lockedWriter{&mu, &b}, nil))
	return func() string { mu.Lock(); defer mu.Unlock(); return b.String() }
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// screens reads the pane stream's screens as they come.
func screens(t *testing.T, body io.Reader) <-chan string {
	out := make(chan string, 100)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var h struct{ Screen string }
				json.Unmarshal([]byte(d), &h)
				out <- h.Screen
			}
		}
	}()
	return out
}

func nextScreen(t *testing.T, ch <-chan string) (string, bool) {
	t.Helper()
	select {
	case s, ok := <-ch:
		return s, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no screen")
	}
	return "", false
}

// The pane stream sends the screen, then the screen again only when it
// changes, takes no input, ends when the pane goes, and answers a pane the
// caller may not read as one that does not exist.
func TestHostScreenStream(t *testing.T) {
	api := &screenAPI{screen: "first screen"}
	var logged func() string
	s := start(t, Config{}, func(s *Server) {
		s.For, s.hostScreenInterval = only(api), 20*time.Millisecond
		logged = logTo(s)
	})
	c := signIn(t, s)

	get := func(pane, body string, header map[string]string) *http.Response {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		hr, _ := http.NewRequest(http.MethodGet, "http://"+s.ln.Addr().String()+HostScreenPath+"?"+url.Values{"pane": {pane}}.Encode(), rd)
		hr.Host = s.base.Host
		hr.AddCookie(c)
		for k, v := range header {
			hr.Header.Set(k, v)
		}
		resp, err := client(s).Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, try := range []struct {
		body   string
		header map[string]string
	}{{"rogue-keys", nil}, {"", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}}} {
		resp := get("%1", try.body, try.header)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("input on the pane route: %s", resp.Status)
		}
	}
	resp := get("%1", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pane %%1: %s", resp.Status)
	}
	ch := screens(t, resp.Body)
	if got, _ := nextScreen(t, ch); got != "first screen" {
		t.Fatalf("first: %q", got)
	}
	api.mu.Lock()
	reads := api.reads
	api.mu.Unlock()
	for ; ; time.Sleep(10 * time.Millisecond) { // a few unchanged reads
		api.mu.Lock()
		n := api.reads
		api.mu.Unlock()
		if n >= reads+3 {
			break
		}
	}
	api.set("second screen", false)
	if got, _ := nextScreen(t, ch); got != "second screen" {
		t.Fatalf("an unchanged screen was sent again, or the change was not: %q", got)
	}
	api.set("", true)
	if got, ok := nextScreen(t, ch); ok {
		t.Fatalf("the stream went on after the pane went: %q", got)
	}
	resp.Body.Close()
	for _, w := range []string{"input to a terminal dropped", "upgrade of a terminal stream dropped"} {
		if !strings.Contains(logged(), w) {
			t.Errorf("not logged: %q", w)
		}
	}

	var bodies []string
	for _, pane := range []string{"%8", "%9"} {
		resp := get(pane, "", nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("pane %s: %s", pane, resp.Status)
		}
		bodies = append(bodies, string(b))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("a forbidden pane can be told from no pane: %q, %q", bodies[0], bodies[1])
	}
}

// A request that declares a body and sends only part of it gets its
// refusal at once, opens nothing, and its connection is closed.
func TestAPartBodyHoldsNothingOpen(t *testing.T) {
	api := &screenAPI{screen: "x"}
	s := start(t, Config{}, func(s *Server) { s.For = only(api) })
	c := signIn(t, s)
	for _, path := range []string{TerminalPath + "?scope=s&key=t1", HostScreenPath + "?pane=%251"} {
		conn, err := net.Dial("tcp", s.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nContent-Length: 100\r\n\r\nrogue", path, s.base.Host, c.Name, c.Value)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, err := io.ReadAll(conn) // ends only when the server closes the connection
		conn.Close()
		if err != nil || !strings.HasPrefix(string(b), "HTTP/1.1 400") {
			t.Fatalf("%s: %v, %q", path, err, b)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.reads != 0 {
		t.Fatal("a pane was read for a request that sent a body")
	}
}
