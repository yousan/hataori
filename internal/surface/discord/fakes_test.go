package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/surface"
)

// capture reads a capture under testdata/v10.
func capture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/v10/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// payload is the d of a gateway capture, or the body of a REST capture.
func payload(t *testing.T, name string) map[string]any {
	t.Helper()
	c := capture(t, name)
	if c["source"] == "gateway" {
		return c["d"].(map[string]any)
	}
	return c["body"].(map[string]any)
}

func raw(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeREST answers "METHOD /path" with a REST capture: its status, its
// rate-limit headers and its body.
type fakeREST struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string][]reply // served in order; the last one served repeats
	last   map[string]reply
	calls  []string
	bodies []request
}

// request is a request the fake received.
type request struct {
	key         string
	query       string
	contentType string
	body        []byte
}

type reply struct {
	status  int
	headers map[string]string
	body    []byte
}

func newFakeREST(t *testing.T) *fakeREST {
	f := &fakeREST{routes: map[string][]reply{}, last: map[string]reply{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.bodies = append(f.bodies, request{key: key, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), body: b})
		rs := f.routes[key]
		rp := f.last[key]
		if len(rs) > 0 {
			rp = rs[0]
			f.routes[key] = rs[1:]
			f.last[key] = rp
		}
		f.mu.Unlock()
		if rp.status == 0 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "Unknown route in the fake", "code": 0}`))
			return
		}
		for k, v := range rp.headers {
			w.Header().Set(k, v)
		}
		if rp.body != nil {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rp.status)
		w.Write(rp.body)
	}))
	t.Cleanup(f.Close)
	return f
}

// serve answers key with the REST capture name.
func (f *fakeREST) serve(t *testing.T, key, name string) {
	t.Helper()
	c := capture(t, name)
	rp := reply{status: int(c["status"].(float64)), headers: map[string]string{}}
	if h, ok := c["headers"].(map[string]any); ok {
		for k, v := range h {
			if k != "content-type" {
				rp.headers[k] = v.(string)
			}
		}
	}
	if c["body"] != nil {
		rp.body = raw(t, c["body"])
	}
	f.mu.Lock()
	f.routes[key] = append(f.routes[key], rp)
	f.mu.Unlock()
}

// serveJSON answers key with v.
func (f *fakeREST) serveJSON(t *testing.T, key string, status int, v any) {
	f.mu.Lock()
	f.routes[key] = append(f.routes[key], reply{status: status, body: raw(t, v)})
	f.mu.Unlock()
}

// requests returns the requests made to key, in order.
func (f *fakeREST) requests(key string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.bodies {
		if r.key == key {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeREST) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fakeGateway speaks enough of the gateway: HELLO, READY on IDENTIFY,
// RESUMED on RESUME, heartbeat ACKs, and the dispatches a test sends.
type fakeGateway struct {
	*httptest.Server
	ready     map[string]any // READY's d
	mu        sync.Mutex
	conn      *websocket.Conn
	seq       int
	identify  int
	resume    int
	connected chan struct{} // a session is ready (READY or RESUMED sent)
	// refuseResume answers the next RESUME with an invalid session that may
	// not be resumed, so that the client identifies again.
	refuseResume bool
}

func newFakeGateway(t *testing.T, ready map[string]any) *fakeGateway {
	g := &fakeGateway{ready: ready, connected: make(chan struct{}, 8)}
	up := websocket.Upgrader{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		g.mu.Lock()
		g.conn = c
		g.mu.Unlock()
		g.send(c, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 41250}})
		for {
			var m struct {
				Op int `json:"op"`
			}
			if err := c.ReadJSON(&m); err != nil {
				return
			}
			switch m.Op {
			case 1:
				g.send(c, map[string]any{"op": 11})
			case 2:
				g.mu.Lock()
				g.identify++
				g.mu.Unlock()
				d := map[string]any{}
				for k, v := range g.ready {
					d[k] = v
				}
				d["resume_gateway_url"] = g.URL()
				g.dispatch(c, "READY", d)
				g.connected <- struct{}{}
			case 6:
				g.mu.Lock()
				refuse := g.refuseResume
				g.refuseResume = false
				if !refuse {
					g.resume++
				}
				g.mu.Unlock()
				if refuse {
					g.send(c, map[string]any{"op": 9, "d": false})
					continue
				}
				g.dispatch(c, "RESUMED", map[string]any{})
				g.connected <- struct{}{}
			}
		}
	}))
	t.Cleanup(func() {
		g.drop()
		g.Close()
	})
	return g
}

// URL is the gateway's websocket address.
func (g *fakeGateway) URL() string { return "ws" + strings.TrimPrefix(g.Server.URL, "http") }

func (g *fakeGateway) send(c *websocket.Conn, v any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c.WriteJSON(v)
}

func (g *fakeGateway) dispatch(c *websocket.Conn, t string, d any) {
	g.mu.Lock()
	g.seq++
	s := g.seq
	g.mu.Unlock()
	g.send(c, map[string]any{"op": 0, "t": t, "s": s, "d": d})
}

// event sends a dispatch on the current connection.
func (g *fakeGateway) event(t string, d any) {
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	g.dispatch(c, t, d)
}

// drop ends the current connection without a close frame, as a network
// outage does.
func (g *fakeGateway) drop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn != nil {
		g.conn.UnderlyingConn().Close()
		g.conn = nil
	}
}

// sink records what the driver delivers.
type sink struct {
	mu       sync.Mutex
	renamed  []renaming
	messages []surface.Inbound
	choices  []surface.Selection
	commands []surface.Command
	layouts  int // calls of LayoutChanged
	got      chan struct{}
}

func (s *sink) LayoutChanged() {
	s.mu.Lock()
	s.layouts++
	s.mu.Unlock()
}

func newSink() *sink { return &sink{got: make(chan struct{}, 64)} }

// renaming is one call of Renamed.
type renaming struct {
	conv  surface.Conversation
	name  string
	after int // messages delivered before it
}

func (s *sink) Renamed(conv surface.Conversation, name string) {
	s.mu.Lock()
	s.renamed = append(s.renamed, renaming{conv, name, len(s.messages)})
	s.mu.Unlock()
}

func (s *sink) Message(in surface.Inbound) {
	s.mu.Lock()
	s.messages = append(s.messages, in)
	s.mu.Unlock()
	s.got <- struct{}{}
}

func (s *sink) Choice(c surface.Selection) {
	s.mu.Lock()
	s.choices = append(s.choices, c)
	s.mu.Unlock()
	s.got <- struct{}{}
}

func (s *sink) Command(c surface.Command) {
	s.mu.Lock()
	s.commands = append(s.commands, c)
	s.mu.Unlock()
	s.got <- struct{}{}
}

func (s *sink) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.messages {
		out = append(out, m.Text)
	}
	return out
}
