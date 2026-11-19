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

// start serves cfg and returns the server. A config with no listener gets
// a loopback one; a listener with no address listens on a free loopback
// port. Each tune changes the server before it serves.
func start(t *testing.T, cfg Config, tune ...func(*Server)) *Server {
	t.Helper()
	if cfg.Local == nil && cfg.Web == nil {
		cfg.Local = &Local{}
	}
	if cfg.Local != nil && cfg.Local.Listen == "" {
		l := *cfg.Local
		l.Listen = "127.0.0.1:0"
		cfg.Local = &l
	}
	if cfg.Web != nil && cfg.Web.Listen == "" {
		w := *cfg.Web
		w.Listen = "127.0.0.1:0"
		cfg.Web = &w
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

// first is the server's loopback listener, or its web listener when it has
// no loopback one.
func first(s *Server) *listener {
	if s.local != nil {
		return s.local
	}
	return s.web
}

// client follows no redirect and sends each request to l, whatever its
// Host.
func client(l *listener) *http.Client {
	addr := l.ln.Addr().String()
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

// requestTo makes a request to l, as a reverse proxy would send it: over TLS
// only when l serves TLS itself, with l's own Host.
func requestTo(t *testing.T, l *listener, method, path string, body io.Reader) *http.Request {
	t.Helper()
	scheme := "http"
	if l.way == control.WayWeb && l.proxy == nil {
		scheme = "https"
	}
	hr, err := http.NewRequest(method, scheme+"://"+l.ln.Addr().String()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	hr.Host = l.base.Host
	return hr
}

type req struct {
	l                          *listener // first(s) when nil
	method, path, host, origin string
	token                      string            // the session, sent as the page sends it
	auth                       string            // an Authorization header of the test's own, instead
	cookie                     *http.Cookie      // a cookie, which no longer signs anyone in
	header                     map[string]string // more headers
}

// answer is a response with its body read.
type answer struct {
	*http.Response
	body string
}

func do(t *testing.T, s *Server, r req) answer {
	t.Helper()
	if r.method == "" {
		r.method = http.MethodGet
	}
	l := r.l
	if l == nil {
		l = first(s)
	}
	hr := requestTo(t, l, r.method, r.path, nil)
	if r.host != "" {
		hr.Host = r.host
	}
	if r.origin != "" {
		hr.Header.Set("Origin", r.origin)
	}
	if r.token != "" {
		hr.Header.Set("Authorization", "Bearer "+r.token)
	}
	if r.auth != "" {
		hr.Header.Set("Authorization", r.auth)
	}
	if r.cookie != nil {
		hr.AddCookie(r.cookie)
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	resp, err := client(l).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return answer{resp, string(b)}
}

// trade is r made into the request with which the page trades link's
// token, from its fragment, for a session.
func trade(t *testing.T, link string, r req) req {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Path != SignInPath || u.RawQuery != "" {
		t.Fatalf("link %q is not the sign-in page with its token in the fragment", link)
	}
	v, _ := url.ParseQuery(u.Fragment)
	r.path, r.auth = SessionPath, "Link "+v.Get("t")
	return r
}

// tokenOf is the session in the answer to a trade, or "".
func tokenOf(a answer) string {
	var v struct{ Session string }
	json.Unmarshal([]byte(a.body), &v)
	return v.Session
}

// signIn uses a new link of the owner for way's listener and returns the
// session.
func signIn(t *testing.T, s *Server, way ...control.Way) string {
	t.Helper()
	w := control.Way("")
	if len(way) > 0 {
		w = way[0]
	}
	link, err := s.NewLink(context.Background(), "owner", w)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := s.listenerOf(w)
	a := do(t, s, trade(t, link, req{l: l}))
	token := tokenOf(a)
	if a.StatusCode != http.StatusOK || token == "" {
		t.Fatalf("sign-in link: %s\n%s", a.Status, a.body)
	}
	return token
}

func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

// Every route that is not the page's own files or the sign-in link needs a
// session in the Authorization header: none, a made-up one, one in a
// cookie, one in the address, and one under another scheme are all
// refused. The page's own files hold no data and need none.
func TestEveryDataRouteNeedsASession(t *testing.T) {
	s := start(t, Config{})
	token := signIn(t, s)
	for _, p := range []string{"/events", "/terminal/7", "/no-such-route", "/signin/x", "/api/threads"} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			for name, r := range map[string]req{
				"no session":                 {},
				"a made-up session":          {token: "made-up"},
				"a session cookie":           {cookie: &http.Cookie{Name: "hataori-session", Value: token}},
				"another scheme":             {auth: "Basic " + token},
				"a bare token":               {auth: token},
				"the session in the address": {path: p + "?session=" + url.QueryEscape(token) + "&token=" + url.QueryEscape(token)},
			} {
				r.method = m
				if r.path == "" {
					r.path = p
				}
				if a := do(t, s, r); a.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s with %s: %s, want 401", m, p, name, a.Status)
				}
			}
		}
	}
	for _, p := range []string{"/events", "/terminal/7"} {
		if a := do(t, s, req{path: p, token: token}); a.StatusCode != http.StatusOK {
			t.Errorf("GET %s signed in: %s", p, a.Status)
		}
	}
	for _, p := range []string{"/"} {
		if a := do(t, s, req{path: p}); a.StatusCode != http.StatusOK || !strings.Contains(a.body, `<div id="root">`) {
			t.Errorf("the page's own %s without a session: %s", p, a.Status)
		}
	}
}

// A link signs in once. It is the sign-in page with the token in its
// fragment; the page trades the token for a session, which comes back as
// JSON: no cookie, no redirect, nothing in an address. Opening the link's
// page, as a preview crawler would, uses nothing up.
func TestALinkWorksOnce(t *testing.T) {
	s := start(t, Config{})
	link, _ := s.NewLink(context.Background(), "owner", "")
	u, _ := url.Parse(link)
	if u.RawQuery != "" || !strings.HasPrefix(u.Fragment, "t=") {
		t.Fatalf("link %q: the token is not in the fragment", link)
	}
	// A crawler fetches the address; it never sees the fragment, and the
	// token in a query does nothing either.
	for _, p := range []string{SignInPath, SignInPath + "?" + u.Fragment} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			a := do(t, s, req{method: m, path: p})
			if a.StatusCode != http.StatusOK || (m == http.MethodGet && !strings.Contains(a.body, `<div id="root">`)) {
				t.Fatalf("%s %s: %s", m, p, a.Status)
			}
		}
	}
	if a := do(t, s, trade(t, link, req{method: http.MethodHead})); a.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a HEAD of the trade: %s", a.Status)
	}
	a := do(t, s, trade(t, link, req{}))
	token := tokenOf(a)
	if a.StatusCode != http.StatusOK || token == "" || !strings.HasPrefix(a.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("first trade: %s %s\n%s", a.Status, a.Header.Get("Content-Type"), a.body)
	}
	if c := a.Header.Values("Set-Cookie"); len(c) != 0 {
		t.Fatalf("the trade set a cookie: %q", c)
	}
	if loc := a.Header.Get("Location"); loc != "" {
		t.Fatalf("the trade redirects to %q", loc)
	}
	if strings.Contains(link, token) {
		t.Fatal("the session is in the link")
	}
	if a := do(t, s, req{path: "/events", token: token}); a.StatusCode != http.StatusOK {
		t.Fatalf("the session it handed over: %s", a.Status)
	}
	if a := do(t, s, trade(t, link, req{})); a.StatusCode != http.StatusUnauthorized || tokenOf(a) != "" {
		t.Fatalf("second trade: %s, want 401", a.Status)
	}
	for name, r := range map[string]req{
		"a made-up link":          {path: SessionPath, auth: "Link made-up"},
		"no link":                 {path: SessionPath},
		"the link as a session":   {path: SessionPath, auth: "Bearer x"},
		"the link in the address": {path: SessionPath + "?t=x"},
	} {
		if a := do(t, s, r); a.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %s, want 401", name, a.Status)
		}
	}
}

// No response of the dashboard sets a cookie: not the page, not a sign-in,
// not a route, not a refusal.
func TestNoResponseSetsACookie(t *testing.T) {
	s := start(t, Config{})
	token := signIn(t, s)
	link, _ := s.NewLink(context.Background(), "owner", "")
	for _, r := range []req{
		{path: "/"}, {path: SignInPath}, trade(t, link, req{}), trade(t, link, req{}), {path: "/events", token: token},
		{path: "/events"}, {path: "/", host: "evil.example.com"}, {method: http.MethodPost, path: "/any", token: token},
	} {
		if a := do(t, s, r); len(a.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s %s: Set-Cookie %q", r.method, r.path, a.Header.Values("Set-Cookie"))
		}
	}
}

// A session belongs to the person its link was made for: its requests are
// made with that person's API, and it answers as none once that person may
// no longer sign in. A session of an earlier layout is refused.
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
		link, err := s.NewLink(context.Background(), person, "")
		if err != nil {
			t.Fatal(err)
		}
		token := tokenOf(do(t, s, trade(t, link, req{})))
		if token == "" {
			t.Fatalf("%s: no session", person)
		}
		mu.Lock()
		asked = nil
		mu.Unlock()
		if a := do(t, s, req{path: "/events", token: token}); a.StatusCode != http.StatusOK {
			t.Fatalf("%s signed in: %s", person, a.Status)
		}
		mu.Lock()
		if !reflect.DeepEqual(asked, []string{person}) {
			t.Fatalf("%s's request asked for the API of %q", person, asked)
		}
		allowed[person] = false
		mu.Unlock()
		if a := do(t, s, req{path: "/events", token: token}); a.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s may no longer sign in: %s, want 401", person, a.Status)
		}
	}
	if _, err := s.NewLink(context.Background(), "", ""); err == nil {
		t.Fatal("a link for nobody was made")
	}

	// The earlier layouts: without a person, and the cookie of before,
	// signed under their own labels.
	for _, c := range []struct {
		label  string
		person string
	}{{"hataori dashboard session\x00", ""}, {"hataori dashboard session 2\x00", "owner"}} {
		payload := make([]byte, 24, 24+len(c.person))
		binary.BigEndian.PutUint64(payload[:8], uint64(time.Now().Add(time.Hour).Unix()))
		payload = append(payload, c.person...)
		m := hmac.New(sha256.New, s.key)
		m.Write([]byte(c.label))
		m.Write(payload)
		old := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
		if a := do(t, s, req{path: "/events", token: old}); a.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a session of the layout %q: %s, want 401", c.label, a.Status)
		}
	}
}

func TestAnExpiredLinkIsRefused(t *testing.T) {
	s := start(t, Config{})
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	link, _ := s.NewLink(context.Background(), "owner", "")
	mu.Lock()
	now = now.Add(s.linkTTL)
	mu.Unlock()
	if a := do(t, s, trade(t, link, req{})); a.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired link: %s, want 401", a.Status)
	}
}

// A session of the loopback listener lasts seven days, one of the web
// listener twelve hours (section 7.2).
func TestASessionExpires(t *testing.T) {
	s := start(t, Config{Local: &Local{}, Web: &Web{TLS: selfSigned(t)}})
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	for _, c := range []struct {
		way  control.Way
		l    *listener
		life time.Duration
	}{{control.WayLocal, s.local, 7 * 24 * time.Hour}, {control.WayWeb, s.web, 12 * time.Hour}} {
		mu.Lock()
		start := now
		mu.Unlock()
		token := signIn(t, s, c.way)
		set := func(d time.Duration) { mu.Lock(); now = start.Add(d); mu.Unlock() }
		set(c.life - time.Minute)
		if a := do(t, s, req{l: c.l, path: "/events", token: token}); a.StatusCode != http.StatusOK {
			t.Fatalf("%s: a minute before its end: %s", c.way, a.Status)
		}
		set(c.life)
		if a := do(t, s, req{l: c.l, path: "/events", token: token}); a.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: at its end: %s, want 401", c.way, a.Status)
		}
		set(0)
	}
}

// The loopback listener answers any loopback name with any port, since a
// port forwarded to it over SSH need not keep its number, and nothing
// else. An Origin must be exactly http:// and the request's own Host.
func TestTheLoopbackListenersHostsAndOrigins(t *testing.T) {
	s := start(t, Config{})
	token := signIn(t, s)
	for _, host := range []string{"localhost:9999", "LocalHost:1", "127.0.0.1:65535", "[::1]:8080", "localhost", "127.0.0.1", "[::1]", s.local.ln.Addr().String()} {
		if a := do(t, s, req{path: "/events", host: host, token: token}); a.StatusCode != http.StatusOK {
			t.Errorf("Host %s: %s", host, a.Status)
		}
	}
	for _, host := range []string{"evil.example.com", "evil.example.com:9999", "127.0.0.2:9999", "localhost.:9999",
		"dash.localhost:9999", "0.0.0.0:9999", "127.1:9999", "localhost:x", "localhost:0", "localhost:65536", "localhost:09999", "[::2]:9999"} {
		for _, p := range []string{"/", "/events", "/signin?t=x"} {
			if a := do(t, s, req{path: p, host: host, token: token}); a.StatusCode != http.StatusForbidden {
				t.Errorf("Host %q, %s: %s, want 403", host, p, a.Status)
			}
		}
	}
	for _, o := range []struct {
		host, origin string
		ok           bool
	}{
		{"localhost:9999", "http://localhost:9999", true},
		{"localhost:9999", "http://LOCALHOST:9999", true},
		{"127.0.0.1:80", "http://127.0.0.1", true},
		{"[::1]:9999", "http://[::1]:9999", true},
		{"localhost:9999", "http://localhost:9998", false}, // another loopback port: another service
		{"localhost:9999", "http://127.0.0.1:9999", false},
		{"localhost:9999", "https://localhost:9999", false},
		{"localhost:9999", "http://localhost:9999/x", false},
		{"localhost:9999", "http://localhost", false},
		{"localhost:9999", "http://user@localhost:9999", false},
		{"localhost:9999", "null", false},
		{"localhost:9999", "http://evil.example.com", false},
	} {
		want := http.StatusOK
		if !o.ok {
			want = http.StatusForbidden
		}
		if a := do(t, s, req{path: "/events", host: o.host, origin: o.origin, token: token}); a.StatusCode != want {
			t.Errorf("Host %s, Origin %s: %s, want %d", o.host, o.origin, a.Status, want)
		}
	}
}

// The web listener answers only its configured name, and an Origin only
// when it is exactly that address.
func TestTheWebListenersHostAndOrigin(t *testing.T) {
	s := start(t, Config{Web: &Web{TLS: selfSigned(t)}})
	token := signIn(t, s, control.WayWeb)
	base := s.web.base
	_, port, _ := net.SplitHostPort(base.Host)
	for _, host := range []string{"evil.example.com", "evil.example.com:" + port, "localhost:" + port, "127.0.0.1", "127.0.0.1:1"} {
		for _, p := range []string{"/", "/events", "/signin?t=x"} {
			if a := do(t, s, req{path: p, host: host, token: token}); a.StatusCode != http.StatusForbidden {
				t.Errorf("Host %s, %s: %s, want 403", host, p, a.Status)
			}
		}
	}
	for _, origin := range []string{"http://evil.example.com", "http://" + base.Host, "https://127.0.0.1:1", "https://" + base.Host + "/x", "null"} {
		if a := do(t, s, req{path: "/events", origin: origin, token: token}); a.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %s: %s, want 403", origin, a.Status)
		}
	}
	if a := do(t, s, req{path: "/events", origin: s.URL(control.WayWeb), token: token}); a.StatusCode != http.StatusOK {
		t.Errorf("own Origin: %s", a.Status)
	}
}

// A link, and the session it opens, belong to the listener the link was
// made for. A link presented on the other listener is used up.
func TestALinkAndASessionBelongToTheirListener(t *testing.T) {
	s := start(t, Config{Local: &Local{}, Web: &Web{TLS: selfSigned(t)}})
	other := map[*listener]*listener{s.local: s.web, s.web: s.local}
	for _, way := range []control.Way{control.WayLocal, control.WayWeb} {
		l, _ := s.listenerOf(way)
		link, err := s.NewLink(context.Background(), "owner", way)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(link, l.base.String()+SignInPath) {
			t.Fatalf("%s: link %s", way, link)
		}
		if a := do(t, s, trade(t, link, req{l: other[l]})); a.StatusCode != http.StatusUnauthorized || tokenOf(a) != "" {
			t.Fatalf("%s's link on the other listener: %s, want 401", way, a.Status)
		}
		if a := do(t, s, trade(t, link, req{l: l})); a.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s's link after it was tried on the other listener: %s, want 401", way, a.Status)
		}
		token := signIn(t, s, way)
		if a := do(t, s, req{l: l, path: "/events", token: token}); a.StatusCode != http.StatusOK {
			t.Fatalf("%s's session: %s", way, a.Status)
		}
		if a := do(t, s, req{l: other[l], path: "/events", token: token}); a.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s's session on the other listener: %s, want 401", way, a.Status)
		}
	}
}

// A link is for the listener asked for; with none named, the loopback
// listener when there is one, else the web listener; a listener the
// dashboard does not have is an error that names its config key.
func TestNewLinkPicksTheListener(t *testing.T) {
	both := start(t, Config{Local: &Local{}, Web: &Web{TLS: selfSigned(t)}})
	local := start(t, Config{Local: &Local{}})
	web := start(t, Config{Web: &Web{TLS: selfSigned(t)}})
	for _, c := range []struct {
		s    *Server
		way  control.Way
		want *listener
		err  string
	}{
		{both, "", both.local, ""}, {both, control.WayLocal, both.local, ""}, {both, control.WayWeb, both.web, ""},
		{local, "", local.local, ""}, {local, control.WayWeb, nil, `"dashboard.web"`},
		{web, "", web.web, ""}, {web, control.WayLocal, nil, `"dashboard.listen"`},
		{both, "lan", nil, "not a listener"},
	} {
		link, err := c.s.NewLink(context.Background(), "owner", c.way)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: %v, want %s", c.way, err, c.err)
			}
			continue
		}
		if err != nil || !strings.HasPrefix(link, c.want.base.String()+SignInPath+"#t=") {
			t.Errorf("%q: %s, %v", c.way, link, err)
		}
	}
}

func TestOnlyReadsAreServed(t *testing.T) {
	s := start(t, Config{})
	token := signIn(t, s)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
		for _, p := range []string{"/", "/events", "/any", "/no-such-route", "/signin?t=x"} {
			if a := do(t, s, req{method: m, path: p, token: token}); a.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %s, want 405", m, p, a.Status)
			}
		}
	}
}

// The loopback listener must be on loopback; the web listener must be
// served over HTTPS, its own or a reverse proxy's. Each refusal names its
// key, with the prefix of the form the config used.
func TestListenersRefuseWhatTheyCannotServe(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "[::]:0", "dash.example.com:0"} {
		_, err := Listen(Config{Local: &Local{Listen: listen}}, nil, slog.Default())
		if err == nil || !strings.Contains(err.Error(), `"dashboard.listen"`) || !strings.Contains(err.Error(), "not a loopback address") ||
			!strings.Contains(err.Error(), `"dashboard.web"`) {
			t.Errorf("loopback listener on %s: %v", listen, err)
		}
	}
	tlsCert := selfSigned(t)
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, "neither a loopback nor a web listener"},
		{Config{Local: &Local{Listen: "nonsense"}}, "is not a host and port"},
		{Config{Local: &Local{Listen: "127.0.0.1:0", URL: "https://localhost:7466"}}, `"dashboard.url": "https://localhost:7466" is not an http address on a loopback name`},
		{Config{Local: &Local{Listen: "127.0.0.1:0", URL: "http://dash.example.com"}}, "not an http address on a loopback name"},
		{Config{Web: &Web{Listen: "127.0.0.1:0"}}, `"dashboard.web.listen": the web listener on "127.0.0.1:0" is served neither with TLS`},
		{Config{Web: &Web{Listen: "127.0.0.1:0", Key: "dashboard."}}, `"dashboard.listen"`},
		{Config{Web: &Web{Listen: "127.0.0.1:0", Proxy: "127.0.0.1"}}, `"dashboard.web.url": missing`},
		{Config{Web: &Web{Listen: "127.0.0.1:0", Proxy: "127.0.0.1", URL: "http://dash.example.com"}}, "not https"},
		{Config{Web: &Web{Listen: "127.0.0.1:0", Proxy: "proxy.example.com", URL: "https://dash.example.com"}}, "not an IP address"},
		{Config{Web: &Web{Listen: "127.0.0.1:0", TLS: tlsCert, URL: "http://dash.example.com"}}, "not https"},
		{Config{Web: &Web{Listen: "127.0.0.1:0", TLS: tlsCert, URL: "https://dash.example.com/path"}}, "is not an address"},
		{Config{Web: &Web{Listen: "0.0.0.0:0", TLS: tlsCert}}, `"dashboard.web.url": missing`},
		// A proxy on another machine would get links and sessions in the clear.
		{Config{Web: &Web{Listen: "0.0.0.0:0", Proxy: "192.0.2.1", URL: "https://dash.example.com"}}, "on another machine"},
		{Config{Web: &Web{Listen: "0.0.0.0:0", URL: "https://dash.example.com"}}, "served neither with TLS"},
	} {
		if _, err := Listen(c.cfg, nil, slog.Default()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.want)
		}
	}
	// Both on one address: the second cannot listen, and the first is let go.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if _, err := Listen(Config{Local: &Local{Listen: addr}, Web: &Web{Listen: addr, TLS: tlsCert}}, nil, slog.Default()); err == nil {
		t.Fatal("two listeners on one address")
	}
	if l, err := net.Listen("tcp", addr); err != nil {
		t.Fatalf("the loopback listener was kept after the web one failed: %v", err)
	} else {
		l.Close()
	}
}

func TestTheWebListenerOverTLS(t *testing.T) {
	s := start(t, Config{Web: &Web{TLS: selfSigned(t)}})
	if !strings.HasPrefix(s.URL(control.WayWeb), "https://") || s.URL(control.WayLocal) != "" {
		t.Fatalf("URLs %q, %q", s.URL(control.WayWeb), s.URL(control.WayLocal))
	}
	token := signIn(t, s, control.WayWeb)
	if a := do(t, s, req{path: "/events", token: token}); a.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %s", a.Status)
	}
}

func TestBehindAProxy(t *testing.T) {
	// The proxy speaks plain HTTP to the dashboard and forwards the Host it
	// was asked for.
	s := start(t, Config{Web: &Web{Proxy: "127.0.0.1", URL: "https://Dash.Example.com:443/"}})
	if s.URL(control.WayWeb) != "https://dash.example.com" {
		t.Fatalf("URL %s", s.URL(control.WayWeb))
	}
	link, _ := s.NewLink(context.Background(), "owner", "")
	if !strings.HasPrefix(link, "https://dash.example.com/signin#t=") {
		t.Fatalf("link %s", link)
	}
	a := do(t, s, trade(t, link, req{host: "dash.example.com"}))
	token := tokenOf(a)
	if a.StatusCode != http.StatusOK || token == "" {
		t.Fatalf("sign-in: %s", a.Status)
	}
	if a := do(t, s, req{path: "/events", host: "dash.example.com", origin: "https://dash.example.com", token: token}); a.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %s", a.Status)
	}
	if a := do(t, s, req{path: "/events", host: s.web.ln.Addr().String(), token: token}); a.StatusCode != http.StatusForbidden {
		t.Fatalf("the listener's own address as Host: %s, want 403", a.Status)
	}
}

func TestOnlyTheProxyMayConnect(t *testing.T) {
	s := start(t, Config{Web: &Web{Proxy: "192.0.2.1", URL: "https://dash.example.com"}})
	if a := do(t, s, req{path: "/", host: "dash.example.com"}); a.StatusCode != http.StatusForbidden {
		t.Fatalf("not from the proxy: %s, want 403", a.Status)
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
func openStream(t *testing.T, s *Server, token string) io.ReadCloser {
	t.Helper()
	l := first(s)
	hr := requestTo(t, l, http.MethodGet, EventsPath, nil)
	hr.Header.Set("Authorization", "Bearer "+token)
	resp, err := client(l).Do(hr)
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
	s := start(t, Config{LocalSessionTTL: 2 * time.Second}, func(s *Server) { s.For = only(streamAPI{ended: ended}) })
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
		hr := requestTo(t, first(s), http.MethodGet, HostScreenPath+"?"+url.Values{"pane": {pane}}.Encode(), rd)
		hr.Header.Set("Authorization", "Bearer "+c)
		for k, v := range header {
			hr.Header.Set(k, v)
		}
		resp, err := client(first(s)).Do(hr)
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
	for _, path := range []string{TerminalPath + "?thread=1", HostScreenPath + "?pane=%251"} {
		conn, err := net.Dial("tcp", first(s).ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: 100\r\n\r\nrogue", path, first(s).base.Host, c)
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

// Closing a server that never served lets go of its ports.
func TestCloseBeforeServeFreesThePorts(t *testing.T) {
	s, err := Listen(Config{Local: &Local{Listen: "127.0.0.1:0"}, Web: &Web{Listen: "127.0.0.1:0", TLS: selfSigned(t)}}, only(noAPI{}), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	addrs := []string{s.local.ln.Addr().String(), s.web.ln.Addr().String()}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		l, err := net.Listen("tcp", a)
		if err != nil {
			t.Fatalf("%s is still held: %v", a, err)
		}
		l.Close()
	}
}

// The loopback listener refuses a request that came through a reverse
// proxy: one that rewrote the Host to a loopback name would otherwise
// open it to whoever the proxy serves. The web listener's proxy is fine.
func TestTheLoopbackListenerRefusesAProxy(t *testing.T) {
	s := start(t, Config{})
	token := signIn(t, s)
	for _, h := range proxyHeaders {
		for _, p := range []string{"/", "/events", SessionPath} {
			if a := do(t, s, req{path: p, token: token, header: map[string]string{h: "192.0.2.1"}}); a.StatusCode != http.StatusForbidden {
				t.Errorf("%s through a proxy (%s): %s, want 403", p, h, a.Status)
			}
		}
	}
	w := start(t, Config{Web: &Web{Proxy: "127.0.0.1", URL: "https://dash.example.com"}})
	wt := tokenOf(do(t, w, trade(t, mustLink(t, w, control.WayWeb), req{host: "dash.example.com"})))
	if a := do(t, w, req{path: "/events", host: "dash.example.com", token: wt,
		header: map[string]string{"X-Forwarded-For": "192.0.2.1", "Via": "1.1 proxy"}}); a.StatusCode != http.StatusOK {
		t.Fatalf("the web listener through its proxy: %s", a.Status)
	}
}

func mustLink(t *testing.T, s *Server, way control.Way) string {
	t.Helper()
	link, err := s.NewLink(context.Background(), "owner", way)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

// A session that would outlast its listener's lifetime as it is now, such
// as one made before the lifetime was shortened, is refused.
func TestASessionIsCappedAtItsListenersLifetime(t *testing.T) {
	s := start(t, Config{Local: &Local{}, LocalSessionTTL: time.Hour})
	ok, _ := s.newSession(time.Now().Add(time.Hour), "owner", control.WayLocal)
	long, _ := s.newSession(time.Now().Add(7*24*time.Hour), "owner", control.WayLocal)
	if a := do(t, s, req{path: "/events", token: ok}); a.StatusCode != http.StatusOK {
		t.Fatalf("a session within the lifetime: %s", a.Status)
	}
	if a := do(t, s, req{path: "/events", token: long}); a.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session longer than the lifetime: %s, want 401", a.Status)
	}
}
