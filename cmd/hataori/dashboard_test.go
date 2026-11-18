package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/wire"
)

// withDashboard adds a "dashboard" section to root's config.
func withDashboard(t *testing.T, root string, section map[string]any) {
	t.Helper()
	p := filepath.Join(root, instance.ConfigFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["dashboard"] = section
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// `hataori signin` prints a link that signs in once.
func TestSignInThroughTheDaemon(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)

	code, out, errOut := cli(t, "signin", "--root", root)
	ls := lines(t, out)
	if code != 0 || len(ls) != 1 {
		t.Fatalf("signin: exit %d, %q, %q", code, out, errOut)
	}
	link, _ := ls[0]["link"].(string)
	if !strings.HasPrefix(link, "http://127.0.0.1:") {
		t.Fatalf("link %q", link)
	}
	base := link[:strings.Index(link, "/signin")]
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(url string, cookies ...*http.Cookie) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		resp, err := hc.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get(base + "/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("before sign-in: %s", resp.Status)
	}
	resp := get(link)
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 || !resp.Cookies()[0].HttpOnly {
		t.Fatalf("sign-in: %s, cookies %v", resp.Status, resp.Header["Set-Cookie"])
	}
	cookie := resp.Cookies()[0]
	if resp := get(base+"/", cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %s", resp.Status)
	}
	if resp := get(link); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the link a second time: %s", resp.Status)
	}
}

// Without a dashboard, signin says so; with an address the dashboard
// refuses, the daemon does not start and says why.
func TestSignInWithoutADashboard(t *testing.T) {
	root := newRoot(t)
	startDaemon(t, root)
	if code, _, errOut := cli(t, "signin", "--root", root); code != exitError || !strings.Contains(errOut, "no dashboard") {
		t.Fatalf("signin: exit %d, %q", code, errOut)
	}
}

func TestDaemonRefusesAnOpenDashboard(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "0.0.0.0:0"})
	code, _, errOut := cli(t, "run", "--root", root)
	var reason string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.HasPrefix(l, "hataori: ") {
			reason = l
		}
	}
	if code != exitError || !strings.Contains(reason, `"dashboard.listen"`) || !strings.Contains(reason, "not a loopback address") {
		t.Fatalf("run: exit %d, %q", code, errOut)
	}
}

// freePort returns a loopback port that nothing listens on now, so that a
// dashboard keeps its address across a restart.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// runDaemon runs the daemon of root in this process until the returned
// function is called; that function waits for it to end.
func runDaemon(t *testing.T, root string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var log bytes.Buffer
	var mu sync.Mutex
	go func() {
		runCtx(ctx, []string{"run", "--root", root}, io.Discard, lockedWriter{&mu, &log})
		close(done)
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(func() { stop(); killSessions(root) })
	eventually(t, "the daemon to serve", 10*time.Second, func() bool {
		c, err := wire.Dial(root)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return stop
}

// dashSignIn signs in with a link from `hataori signin` and returns the
// dashboard's address and the session cookie.
func dashSignIn(t *testing.T, root string) (string, *http.Cookie) {
	t.Helper()
	_, out, errOut := cli(t, "signin", "--root", root)
	ls := lines(t, out)
	if len(ls) != 1 {
		t.Fatalf("signin: %q %q", out, errOut)
	}
	link := ls[0]["link"].(string)
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resp.Cookies()) != 1 {
		t.Fatalf("sign-in: %s", resp.Status)
	}
	return link[:strings.Index(link, "/signin")], resp.Cookies()[0]
}

// dashGet returns the status and body of a GET of a dashboard route.
func dashGet(t *testing.T, base string, c *http.Cookie, path string) (int, string) {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, base+path, nil)
	r.AddCookie(c)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// sseEvent is one message of the dashboard's event stream.
type sseEvent struct {
	id    uint64
	kind  string // "caught_up" for the mark, else empty
	event control.Event
	raw   string
	at    time.Time // when it was read
}

// dashStream reads the event stream after lastID until the test ends or
// stop is called. Messages arrive on the returned channel.
func dashStream(t *testing.T, base string, c *http.Cookie, lastID uint64) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events", nil)
	r.AddCookie(c)
	if lastID > 0 {
		r.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		t.Fatalf("event stream: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	out := make(chan sseEvent, 1000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var m sseEvent
		var block []string
		for sc.Scan() {
			l := sc.Text()
			if l != "" {
				block = append(block, l)
				switch {
				case strings.HasPrefix(l, "id: "):
					m.id, _ = strconv.ParseUint(l[4:], 10, 64)
				case strings.HasPrefix(l, "event: "):
					m.kind = l[7:]
				case strings.HasPrefix(l, "data: "):
					json.Unmarshal([]byte(l[6:]), &m.event)
				}
				continue
			}
			if m.id != 0 || m.kind != "" {
				m.raw, m.at = strings.Join(block, "\n"), time.Now()
				out <- m
			}
			m, block = sseEvent{}, nil
		}
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return out, stop
}

// next returns the next message of a stream, or fails after timeout.
func next(t *testing.T, ch <-chan sseEvent, timeout time.Duration) sseEvent {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("the event stream ended")
		}
		return m
	case <-time.After(timeout):
		t.Fatal("no event within", timeout)
	}
	return sseEvent{}
}

// A page that reconnects with the last position it saw gets every later
// event once, also when the daemon restarted in between; a live event
// reaches the stream within 2 seconds of being written.
func TestDashboardEventStreamResumesAcrossARestart(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": freePort(t)})
	stop := runDaemon(t, root)
	base, cookie := dashSignIn(t, root)
	ctx := context.Background()

	ch, closeStream := dashStream(t, base, cookie, 0)
	if m := next(t, ch, 5*time.Second); m.kind != "caught_up" || m.id != 0 {
		t.Fatalf("an empty instance's stream starts with %q", m.raw)
	}
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t),
		Input: control.Input{Text: "reply one"}})
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	var seen []uint64
	live := func(m sseEvent) {
		t.Helper()
		if d := m.at.Sub(m.event.Time); d > 2*time.Second {
			t.Errorf("event %d (%s) reached the stream %v after it was written", m.id, m.event.Kind, d)
		}
	}
	for {
		m := next(t, ch, 15*time.Second)
		if m.kind != "" || m.id != m.event.Pos || m.event.Thread != id {
			t.Fatalf("unexpected message %q", m.raw)
		}
		live(m)
		seen = append(seen, m.id)
		if m.event.Kind == "turn_ended" {
			break
		}
	}
	closeStream()

	stop()
	runDaemon(t, root)
	c, err = wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch, _ = dashStream(t, base, cookie, seen[len(seen)-1]) // the session outlived the restart
	if err := c.Send(ctx, id, "k2", control.Input{Text: "reply two"}, control.Deliver); err != nil {
		t.Fatal(err)
	}
	caught := false
	for turns := 0; turns < 1; {
		m := next(t, ch, 15*time.Second)
		if m.kind == "caught_up" {
			caught = true
			continue
		}
		if caught {
			live(m)
		}
		seen = append(seen, m.id)
		if m.event.Kind == "turn_ended" {
			turns++
		}
	}
	if !caught {
		t.Fatal("no caught_up mark after the reconnect")
	}

	// Every event up to the last one read, each once, in order.
	_, out, _ := cli(t, "events", "--root", root)
	var want []uint64
	for _, e := range lines(t, out) {
		if p := uint64(e["pos"].(float64)); p <= seen[len(seen)-1] {
			want = append(want, p)
		}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("positions read across the restart:\n%v\nwant\n%v", seen, want)
	}
}

// A thread outside the dashboard caller's scopes, and its events, are never
// sent, and neither are text fields a caller that may not read text gets.
func TestDashboardSendsOnlyWhatTheCallerMayRead(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	p := filepath.Join(root, instance.ConfigFile)
	b, _ := os.ReadFile(p)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["scopes"] = []map[string]any{{"key": "s", "client": "chat", "enabled": true}, {"key": "x", "client": "chat", "enabled": true}}
	b, _ = json.Marshal(cfg)
	os.WriteFile(p, b, 0o600)
	dashboardHook = func(c *control.Caller) { c.Scopes, c.NoText = []string{"s"}, true }
	t.Cleanup(func() { dashboardHook = nil })
	runDaemon(t, root)
	base, cookie := dashSignIn(t, root)

	ctx := context.Background()
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	repo := bareRepo(t)
	for _, s := range []control.StartRequest{
		{Key: "k1", Scope: "s", Thread: "t1", Repo: repo, Input: control.Input{Text: "reply hello\nreply secret-one"}},
		{Key: "k2", Scope: "x", Thread: "t2", Repo: repo, Input: control.Input{Text: "reply secret-two"}},
	} {
		if _, err := c.Start(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "both turns to end", 15*time.Second, func() bool {
		_, out, _ := cli(t, "events", "--root", root)
		return strings.Count(out, `"kind":"turn_ended"`) == 2
	})

	var bodies []string
	code, threads := dashGet(t, base, cookie, "/api/threads")
	if code != http.StatusOK || !strings.Contains(threads, `"key":"t1"`) || strings.Contains(threads, `"t2"`) {
		t.Fatalf("threads: %d %s", code, threads)
	}
	bodies = append(bodies, threads)
	code, one := dashGet(t, base, cookie, "/api/thread?scope=s&key=t1&events=100")
	if code != http.StatusOK || !strings.Contains(one, `"kind":"text"`) {
		t.Fatalf("thread t1: %d %s", code, one)
	}
	bodies = append(bodies, one)
	if code, body := dashGet(t, base, cookie, "/api/thread?scope=x&key=t2"); code != http.StatusForbidden {
		t.Fatalf("thread t2 in another scope: %d %s", code, body)
	}

	ch, _ := dashStream(t, base, cookie, 0)
	texts := 0
	for {
		m := next(t, ch, 10*time.Second)
		bodies = append(bodies, m.raw)
		if m.kind == "caught_up" {
			break
		}
		if m.event.Thread.Scope != "s" {
			t.Fatalf("an event of another scope: %s", m.raw)
		}
		if m.event.Kind == "text" {
			texts++
		}
	}
	if texts == 0 {
		t.Fatal("no text event: the stream sent nothing to check")
	}
	for _, b := range bodies {
		if strings.Contains(b, "secret") {
			t.Fatalf("text the caller may not read was sent: %s", b)
		}
	}
}

// A session key that other users may read is refused: whoever reads it can
// make sessions.
func TestDaemonRefusesAReadableSessionKey(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	runDaemon(t, root)() // makes the key, then stops
	key := filepath.Join(root, sessionKeyFile)
	if st, err := os.Stat(key); err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if _, err := os.Stat(key + ".new"); !os.IsNotExist(err) {
		t.Fatalf("the temporary key file is left: %v", err)
	}
	os.Chmod(key, 0o640)
	// A daemon that starts anyway is stopped after a while, and fails the test.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errOut bytes.Buffer
	code := runCtx(ctx, []string{"run", "--root", root}, io.Discard, &errOut)
	t.Cleanup(func() { killSessions(root) })
	if code != exitError || !strings.Contains(errOut.String(), "accessible to other users") {
		t.Fatalf("run: exit %d, %q", code, errOut.String())
	}
}
