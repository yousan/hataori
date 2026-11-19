package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// forward passes every connection to its own loopback port on to target,
// as `ssh -L` does on the person's machine, and returns the port.
func forward(t *testing.T, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				d, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer d.Close()
				done := make(chan struct{}, 2)
				go func() { io.Copy(d, c); done <- struct{}{} }()
				go func() { io.Copy(c, d); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// signinLink runs `hataori signin` with args and returns the link.
func signinLink(t *testing.T, root string, args ...string) string {
	t.Helper()
	code, out, errOut := cli(t, append([]string{"signin", "--root", root}, args...)...)
	ls := lines(t, out)
	if code != 0 || len(ls) != 1 {
		t.Fatalf("signin %v: exit %d, %q, %q", args, code, out, errOut)
	}
	return ls[0]["link"].(string)
}

// onPort is link with its host and port changed to localhost:port, as the
// person opens it on their machine after forwarding that port.
func onPort(t *testing.T, link, port string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + port
	return u.String()
}

// startThread starts a thread in scope s of root that replies with text.
func startThread(t *testing.T, root, key, text string) {
	t.Helper()
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Start(context.Background(), control.StartRequest{Key: "k-" + key, Scope: "s", Thread: key, Repo: bareRepo(t),
		Input: control.Input{Text: text}}); err != nil {
		t.Fatal(err)
	}
}

// The loopback listener through a port with another number, as SSH
// forwards it: the link with the port changed signs in, the routes and the
// event stream answer through it, and a foreign Host or Origin is refused
// there too.
func TestDashboardThroughAForwardedPort(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	startThread(t, root, "t1", "reply forwarded")

	link := signinLink(t, root)
	daemon, err := url.Parse(link)
	if err != nil || daemon.Scheme != "http" {
		t.Fatalf("link %q", link)
	}
	port := forward(t, daemon.Host)
	if _, p, _ := net.SplitHostPort(daemon.Host); p == port {
		t.Fatal("the forwarded port has the daemon's number")
	}
	base := "http://localhost:" + port
	session := openLink(t, onPort(t, link, port))
	eventually(t, "the thread through the forwarded port", 10*time.Second, func() bool {
		code, body := dashGet(t, base, session, "/api/threads")
		return code == http.StatusOK && strings.Contains(body, "reply forwarded")
	})
	events, stop := dashStream(t, base, session, 0)
	defer stop()
	select {
	case m := <-events:
		if m.id == 0 && m.kind == "" {
			t.Fatalf("not an event: %s", m.raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no event through the forwarded port")
	}

	for _, c := range []struct {
		host, origin string
		want         int
	}{
		{"localhost:" + port, "http://localhost:" + port, http.StatusOK},
		{"127.0.0.1:" + port, "", http.StatusOK},
		{"evil.example.com:" + port, "", http.StatusForbidden},
		{"dash.localhost:" + port, "", http.StatusForbidden},
		{"localhost:" + port, "http://localhost:1", http.StatusForbidden}, // another loopback port: another service
		{"localhost:" + port, "http://127.0.0.1:" + port, http.StatusForbidden},
		{"localhost:" + port, "http://evil.example.com", http.StatusForbidden},
		{"localhost:" + port, "null", http.StatusForbidden},
	} {
		if code, body := rawGet(t, "GET", base, "/api/threads", session, c.host, c.origin); code != c.want {
			t.Errorf("Host %s, Origin %q: %d %s, want %d", c.host, c.origin, code, body, c.want)
		}
	}
	// A session in a cookie, as before, signs nobody in.
	r, _ := http.NewRequest(http.MethodGet, base+"/api/threads", nil)
	r.AddCookie(&http.Cookie{Name: "hataori-session", Value: string(session)})
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the session in a cookie: %s, want 401", resp.Status)
	}
}

// writeCert writes a self-signed certificate for 127.0.0.1 and its key
// into root's config directory.
func writeCert(t *testing.T, root string) {
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
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(root, "config", "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(root, "config", "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

// insecure is a client for the test's own self-signed web listener.
var insecure = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

// A daemon with both listeners: `hataori signin` gives the loopback
// listener's link and `--web` the web listener's; a link presented on the
// other listener signs nobody in and is used up, and a session of one
// listener is refused on the other.
func TestDashboardLinksAndSessionsStayOnTheirListener(t *testing.T) {
	root := newRoot(t)
	writeCert(t, root)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0",
		"web": map[string]any{"listen": "127.0.0.1:0", "tls_cert": "cert.pem", "tls_key": "key.pem"}})
	logged := startDaemonLog(t, root)
	startThread(t, root, "t1", "reply on both")

	local, web := signinLink(t, root), signinLink(t, root, "--web")
	if !strings.HasPrefix(local, "http://127.0.0.1:") || !strings.HasPrefix(web, "https://127.0.0.1:") {
		t.Fatalf("links %q, %q", local, web)
	}
	baseOf := func(link string) string { return link[:strings.Index(link, "/signin")] }
	clientOf := func(link string) *http.Client {
		if strings.HasPrefix(link, "https:") {
			return insecure
		}
		return &http.Client{}
	}
	other := map[string]string{local: baseOf(web), web: baseOf(local)}
	// otherBase is the other listener's address for a link of one of them.
	otherBase := func(link string) string {
		if strings.HasPrefix(link, "https:") {
			return baseOf(local)
		}
		return baseOf(web)
	}
	for _, link := range []string{local, web} {
		u, _ := url.Parse(link)
		wrong := other[link] + u.Path + "#" + u.Fragment
		if code, _, s := tryLink(t, clientOf(wrong), wrong); code != http.StatusUnauthorized || s != "" {
			t.Fatalf("%s on the other listener: %d, want 401", link, code)
		}
		if code, _, _ := tryLink(t, clientOf(link), link); code != http.StatusUnauthorized {
			t.Fatalf("%s after it was tried on the other listener: %d, want 401", link, code)
		}
	}
	for _, args := range [][]string{nil, {"--web"}} {
		link := signinLink(t, root, args...)
		hc := clientOf(link)
		code, body, session := tryLink(t, hc, link)
		if code != http.StatusOK || session == "" {
			t.Fatalf("signin %v: %d %s", args, code, body)
		}
		get := func(hc *http.Client, base string) int {
			r, _ := http.NewRequest(http.MethodGet, base+"/api/threads", nil)
			session.set(r)
			resp, err := hc.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		if c := get(hc, baseOf(link)); c != http.StatusOK {
			t.Fatalf("signin %v: its own listener: %d", args, c)
		}
		ob := otherBase(link)
		if c := get(clientOf(ob), ob); c != http.StatusUnauthorized {
			t.Fatalf("signin %v: the other listener: %d, want 401", args, c)
		}
	}
	if l := logged(); strings.Contains(l, "dashboard section uses the old form") {
		t.Fatalf("a config in the new form was logged as the old form: %s", l)
	}
}

// Each listener's link fails, naming its config key, when the instance has
// no such listener.
func TestSignInForAListenerThatIsNotThere(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	if code, _, errOut := cli(t, "signin", "--web", "--root", root); code != exitError || !strings.Contains(errOut, `"dashboard.web"`) {
		t.Fatalf("signin --web without a web listener: exit %d, %q", code, errOut)
	}

	root = newRoot(t)
	writeCert(t, root)
	withDashboard(t, root, map[string]any{"web": map[string]any{"listen": "127.0.0.1:0", "tls_cert": "cert.pem", "tls_key": "key.pem"}})
	startDaemon(t, root)
	if code, _, errOut := cli(t, "signin", "--root", root); code != exitError || !strings.Contains(errOut, `"dashboard.listen"`) || !strings.Contains(errOut, "--web") {
		t.Fatalf("signin without a loopback listener: exit %d, %q", code, errOut)
	}
	if link := signinLink(t, root, "--web"); !strings.HasPrefix(link, "https://127.0.0.1:") {
		t.Fatalf("signin --web: %q", link)
	}
	if code, _, errOut := cli(t, "threads", "--web", "--root", root); code != exitUsage {
		t.Fatalf("--web on another command: exit %d, %q", code, errOut)
	}
}

// A config in the old form, one listener, is read as before: with a proxy
// it is the web listener alone, at its "url"; without one, the loopback
// listener, whose links point at its "url". The daemon says once that the
// config uses the old form.
func TestDashboardOldOneListenerConfig(t *testing.T) {
	root := newRoot(t)
	listen := freePort(t)
	withDashboard(t, root, map[string]any{"listen": listen, "url": "https://dash.example.com", "proxy": "127.0.0.1"})
	logged := startDaemonLog(t, root)
	link := signinLink(t, root, "--web")
	if !strings.HasPrefix(link, "https://dash.example.com/signin#t=") {
		t.Fatalf("link %q", link)
	}
	if code, _, errOut := cli(t, "signin", "--root", root); code != exitError || !strings.Contains(errOut, "no loopback listener") {
		t.Fatalf("signin with only the old web listener: exit %d, %q", code, errOut)
	}
	// The reverse proxy on this machine forwards the Host it was asked for,
	// over plain HTTP.
	plain := "http" + strings.TrimPrefix(link, "https")
	if code, body, s := tryLinkAt(t, &http.Client{}, plain, listen, "dash.example.com"); code != http.StatusOK || s == "" {
		t.Fatalf("sign-in through the proxy: %d %s", code, body)
	}
	if n := strings.Count(logged(), "dashboard section uses the old form"); n != 1 {
		t.Fatalf("the old form was logged %d times:\n%s", n, logged())
	}

	root = newRoot(t)
	listen = freePort(t)
	_, port, _ := net.SplitHostPort(listen)
	withDashboard(t, root, map[string]any{"listen": listen, "url": "http://localhost:" + port})
	logged = startDaemonLog(t, root)
	link = signinLink(t, root)
	if !strings.HasPrefix(link, "http://localhost:"+port+"/signin#t=") {
		t.Fatalf("link %q", link)
	}
	openLink(t, link)
	if n := strings.Count(logged(), "dashboard section uses the old form"); n != 1 {
		t.Fatalf("the old form was logged %d times:\n%s", n, logged())
	}
}

// In a browser: the page signs in through a forwarded port and shows the
// threads, live; its session is in no cookie and no address; a service on
// another loopback port, opened in the same browser, gets no credential of
// the dashboard; and two instances on two ports of one machine keep their
// sessions apart.
func TestDashboardSessionsInTheBrowser(t *testing.T) {
	roots := []string{newRoot(t), newRoot(t)}
	var ports []string
	for i, root := range roots {
		withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
		startDaemon(t, root)
		startThread(t, root, "t1", fmt.Sprintf("reply instance %d", i))
		u, _ := url.Parse(signinLink(t, root))
		ports = append(ports, forward(t, u.Host))
	}

	// A service of someone else's on another loopback port: it records what
	// the browser sends it.
	var mu sync.Mutex
	var seen []http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		io.WriteString(w, "<!doctype html><p>another service</p>")
	}))
	defer other.Close()
	_, otherPort, _ := net.SplitHostPort(other.Listener.Addr().String())

	b := newBrowser(t)
	b.call("Network.enable", map[string]any{})
	thread := func(i int) string {
		return fmt.Sprintf(`[...document.querySelectorAll('[data-section=threads] .row')].some(r => r.textContent.includes('reply instance %d'))`, i)
	}
	sessions := make([]string, len(roots))
	for i, root := range roots {
		b.call("Page.navigate", map[string]any{"url": onPort(t, signinLink(t, root), ports[i])})
		b.until(fmt.Sprintf("instance %d's thread", i), thread(i))
		b.until("the live mark on, with a position", `(() => { const l = document.querySelector('[data-live]'); return !!l && l.dataset.live === 'on' && l.dataset.pos !== ''; })()`)
		var loc, cookie string
		b.eval(`location.pathname + location.search`, &loc)
		b.eval(`document.cookie`, &cookie)
		b.eval(`localStorage.getItem('hataori-session') || ''`, &sessions[i])
		if loc != "/" || cookie != "" || sessions[i] == "" {
			t.Fatalf("instance %d: address %q, cookie %q, session kept %v", i, loc, cookie, sessions[i] != "")
		}
	}
	b.call("Page.navigate", map[string]any{"url": "http://localhost:" + otherPort + "/"})
	b.until("the other service", `document.body.innerText.includes('another service')`)
	// Back on each instance without a link: each still has its own session.
	for i := range roots {
		b.call("Page.navigate", map[string]any{"url": "http://localhost:" + ports[i] + "/"})
		b.until(fmt.Sprintf("instance %d's thread again", i), thread(i))
		var mixed bool
		b.eval(thread(1-i), &mixed)
		if mixed {
			t.Fatalf("instance %d shows the other instance's thread", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the other service was not asked")
	}
	for _, h := range seen {
		for _, name := range []string{"Cookie", "Authorization"} {
			if v := h.Get(name); v != "" {
				t.Fatalf("the other service got %s: %q", name, v)
			}
		}
	}
	for _, e := range b.events {
		if e.Method != "Network.requestWillBeSent" {
			continue
		}
		var p struct {
			Request struct {
				URL     string
				Headers map[string]string
			}
		}
		json.Unmarshal(e.Params, &p)
		for _, s := range sessions {
			if strings.Contains(p.Request.URL, s) || strings.Contains(p.Request.URL, url.QueryEscape(s)) {
				t.Fatalf("a request carries the session in its address: %s", p.Request.URL)
			}
		}
		for h := range p.Request.Headers {
			if strings.EqualFold(h, "Cookie") {
				t.Fatalf("a request sent a cookie: %s", p.Request.URL)
			}
		}
	}
}

// A link that someone used first does not sign in, and the page says it
// was already used, which a signed-out page does not.
func TestDashboardSaysALinkWasAlreadyUsed(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	link := signinLink(t, root)
	openLink(t, link) // someone else, first
	b := newBrowser(t)
	b.call("Page.navigate", map[string]any{"url": link})
	b.until("the page to say the link was used", `(document.querySelector('.signed-out') || {}).textContent?.includes('already used')`)
	var loc string
	b.eval(`location.pathname + location.hash`, &loc)
	if loc != "/" {
		t.Fatalf("the used link stayed in the address: %q", loc)
	}
	b.call("Page.navigate", map[string]any{"url": strings.TrimSuffix(link[:strings.Index(link, "/signin")], "/") + "/"})
	b.until("the plain signed-out message", `(document.querySelector('.signed-out') || {}).textContent?.includes('Not signed in')`)

	// Signed in with a session of its own, the page still says so when a
	// link it is opened with was used by someone first.
	startThread(t, root, "t1", "reply signed in")
	b.call("Page.navigate", map[string]any{"url": signinLink(t, root)})
	b.until("the thread, signed in", `[...document.querySelectorAll('[data-section=threads] .row')].some(r => r.textContent.includes('reply signed in'))`)
	stolen := signinLink(t, root)
	openLink(t, stolen)
	b.call("Page.navigate", map[string]any{"url": stolen})
	b.until("the warning while still signed in", `(document.querySelector('.signed-out') || {}).textContent?.includes('already used') && [...document.querySelectorAll('[data-section=threads] .row')].some(r => r.textContent.includes('reply signed in'))`)
}
