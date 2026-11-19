package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
)

// opAPI is the owner's control API as the operate route uses it: it starts
// fake operations of pane %1, and reads its screen.
type opAPI struct {
	control.API
	mu      sync.Mutex
	refuse  error
	ops     []*fakeOp
	screens int // HostScreen calls
	views   int // HostView calls
}

func (a *opAPI) OperateHost(ctx context.Context, pane string) (control.HostOperation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refuse != nil {
		return nil, a.refuse
	}
	if pane != "%1" {
		return nil, fmt.Errorf("core: OperateHost: tmux pane %q: %w", pane, core.ErrNotFound)
	}
	o := &fakeOp{done: make(chan struct{})}
	a.ops = append(a.ops, o)
	go func() {
		select {
		case <-ctx.Done():
			o.finish(control.HostClosed)
		case <-o.done:
		}
	}()
	return o, nil
}

func (a *opAPI) HostScreen(_ context.Context, pane string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.screens++
	return "a screen", nil
}

func (a *opAPI) HostView(_ context.Context, pane string) (control.HostView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.views++
	return control.HostView{Screen: "a screen", CursorX: 3, CursorY: 1}, nil
}

func (a *opAPI) started() []*fakeOp {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ops)
}

func (a *opAPI) reads() (screens, views int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.screens, a.views
}

// fakeOp records the batches it is given.
type fakeOp struct {
	mu     sync.Mutex
	keys   [][]control.HostKey
	reason string
	done   chan struct{}
	once   sync.Once
}

func (o *fakeOp) finish(reason string) {
	o.once.Do(func() {
		o.mu.Lock()
		o.reason = reason
		o.mu.Unlock()
		close(o.done)
	})
}

func (o *fakeOp) Send(_ context.Context, keys []control.HostKey) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reason != "" {
		return control.ErrEnded
	}
	if keys[0].Key == "Space" {
		return errors.New("core: input 1 of the batch is neither a key name hataori sends nor a text; nothing was sent")
	}
	o.keys = append(o.keys, keys)
	return nil
}
func (o *fakeOp) Idle() time.Duration   { return 5 * time.Minute }
func (o *fakeOp) Done() <-chan struct{} { return o.done }
func (o *fakeOp) End()                  { o.finish(control.HostEnded) }
func (o *fakeOp) Reason() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reason
}

func (o *fakeOp) batches() [][]control.HostKey {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.keys)
}

// operateServer serves a loopback listener with host input on, its
// sessions the owner's with a.
func operateServer(t *testing.T, a *opAPI, cfg Config, tune ...func(*Server)) *Server {
	t.Helper()
	cfg.HostInput = true
	return start(t, cfg, append([]func(*Server){func(s *Server) { s.For = only(a) }}, tune...)...)
}

// dialOperate opens the operate WebSocket on l with origin, as a browser on
// the listener's page would ("" sends none).
func dialOperate(t *testing.T, l *listener, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	addr := l.ln.Addr().String()
	d := websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		HandshakeTimeout: 5 * time.Second,
	}
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	c, resp, err := d.Dial("ws://"+l.base.Host+OperatePath, h)
	if c != nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, resp, err
}

// origin is the page's Origin on l.
func origin(l *listener) string { return l.base.String() }

// readOut reads the server's next message.
func readOut(t *testing.T, c *websocket.Conn) operateOut {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m operateOut
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("no message: %v", err)
	}
	return m
}

// closedWith reads until the server closes, and returns its close code and
// reason.
func closedWith(t *testing.T, c *websocket.Conn) (int, string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			return ce.Code, ce.Text
		}
		if err != nil {
			t.Fatalf("closed without a close frame: %v", err)
		}
	}
}

func first2(session string) map[string]any {
	return map[string]any{"session": session, "pane": "%1"}
}

// The operate route is the only one that accepts an Upgrade. Its handshake
// is refused with an Origin that is missing, null, another loopback port's
// or another site's, and while host input is off; nothing is started.
func TestOperateHandshake(t *testing.T) {
	a := &opAPI{}
	s := operateServer(t, a, Config{})
	logged := logTo(s)
	l := first(s)
	_, port, _ := net.SplitHostPort(l.base.Host)
	other := "1"
	if port == "1" {
		other = "2"
	}
	for _, o := range []string{"", "null", "http://127.0.0.1:" + other, "http://localhost:" + other, "https://" + l.base.Host, "http://evil.example"} {
		_, resp, err := dialOperate(t, l, o)
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: %v %v", o, resp, err)
		}
	}
	if !strings.Contains(logged(), "the operate connection must carry this listener's origin") {
		t.Errorf("the refusals are not logged:\n%s", logged())
	}
	if c, _, err := dialOperate(t, l, origin(l)); err != nil {
		t.Fatalf("the page's own Origin: %v", err)
	} else {
		c.Close()
	}
	// Another loopback name of the same port is the page under that name.
	if _, _, err := dialOperate(t, l, "http://localhost:"+port); err == nil {
		t.Error("an Origin of another name than the request's Host")
	}

	// An Upgrade on any other route is refused; the screen streams refuse
	// one themselves (TestHostScreenStream).
	tok := signIn(t, s)
	for _, p := range []string{ThreadsPath, HostTerminalsPath, "/any"} {
		got := do(t, s, req{path: p, token: tok, origin: origin(l), header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}})
		if got.StatusCode != http.StatusBadRequest {
			t.Errorf("an upgrade of %s: %s", p, got.Status)
		}
	}

	off := start(t, Config{}, func(s *Server) { s.For = only(a) })
	if _, resp, err := dialOperate(t, first(off), origin(first(off))); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("with host input off: %v %v", resp, err)
	}
	if len(a.started()) != 0 {
		t.Errorf("a refused handshake started %d operations", len(a.started()))
	}
}

// The session comes in the first message, and nothing else is accepted
// before it: a first message that is not a valid session closes the
// WebSocket with the reason, and starts nothing.
func TestOperateFirstMessage(t *testing.T) {
	a := &opAPI{}
	s := operateServer(t, a, Config{})
	l := first(s)
	tok := signIn(t, s)
	someone, err := s.newSession(s.now().Add(time.Hour), "someone", control.WayLocal, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	web, _ := s.newSession(s.now().Add(time.Hour), "owner", control.WayWeb, time.Time{})
	for name, send := range map[string]func(*websocket.Conn) error{
		"keys first": func(c *websocket.Conn) error {
			return c.WriteJSON(map[string]any{"keys": []map[string]string{{"key": "Enter"}}})
		},
		"not JSON": func(c *websocket.Conn) error { return c.WriteMessage(websocket.TextMessage, []byte("session")) },
		"a binary message": func(c *websocket.Conn) error {
			return c.WriteMessage(websocket.BinaryMessage, []byte(`{"session":"`+tok+`","pane":"%1"}`))
		},
		"a made-up session":      func(c *websocket.Conn) error { return c.WriteJSON(first2(tok + "x")) },
		"a session of no person": func(c *websocket.Conn) error { return c.WriteJSON(first2(someone)) },
		"the web's session":      func(c *websocket.Conn) error { return c.WriteJSON(first2(web)) },
		"no pane":                func(c *websocket.Conn) error { return c.WriteJSON(map[string]any{"session": tok}) },
	} {
		c, _, err := dialOperate(t, l, origin(l))
		if err != nil {
			t.Fatal(err)
		}
		if err := send(c); err != nil {
			t.Fatal(err)
		}
		if m := readOut(t, c); m.Refused == "" || m.Started != nil {
			t.Errorf("%s: %+v", name, m)
		}
		if code, why := closedWith(t, c); code != websocket.ClosePolicyViolation || why == "" {
			t.Errorf("%s: closed with %d %q", name, code, why)
		}
	}
	if len(a.started()) != 0 {
		t.Fatalf("%d operations started", len(a.started()))
	}

	// No first message: the WebSocket is closed after firstMessageTimeout.
	c, _, err := dialOperate(t, l, origin(l))
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(firstMessageTimeout + 5*time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Error("a WebSocket with no first message stays open")
	}
}

// On the loopback listener an operation starts at once; the page's
// batches reach it in order, a refused batch is answered and the
// operation goes on, and End ends it: the page gets the reason, and the
// WebSocket closes with it.
func TestOperateOnTheLoopbackListener(t *testing.T) {
	a := &opAPI{}
	s := operateServer(t, a, Config{})
	l := first(s)
	logged := logTo(s)
	c, _, err := dialOperate(t, l, origin(l))
	if err != nil {
		t.Fatal(err)
	}
	c.WriteJSON(first2(signIn(t, s)))
	if m := readOut(t, c); m.Started == nil || m.Started.Idle != 300 {
		t.Fatalf("not started: %+v", m)
	}
	const typed = "hataori-operate-route-marker-3c9a"
	batches := [][]control.HostKey{{{Key: "Up"}}, {{Text: typed}, {Key: "Enter"}}, {{Text: "pasted", Paste: true}}}
	for _, b := range batches {
		c.WriteJSON(map[string]any{"keys": b})
	}
	c.WriteJSON(map[string]any{"keys": []control.HostKey{{Key: "Space"}}})
	if m := readOut(t, c); m.Error == "" {
		t.Fatalf("a refused batch: %+v", m)
	}
	c.WriteJSON(map[string]any{"keys": []control.HostKey{{Key: "Down"}}})
	ops := a.started()
	if len(ops) != 1 {
		t.Fatalf("%d operations", len(ops))
	}
	want := append(batches, []control.HostKey{{Key: "Down"}})
	for deadline := time.Now().Add(5 * time.Second); len(ops[0].batches()) < len(want) && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ops[0].batches(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("batches %+v, want %+v", got, want)
	}
	c.WriteJSON(map[string]any{"end": true})
	if m := readOut(t, c); m.Ended != control.HostEnded {
		t.Fatalf("after End: %+v", m)
	}
	if code, why := closedWith(t, c); code != websocket.CloseNormalClosure || why != control.HostEnded {
		t.Errorf("closed with %d %q", code, why)
	}
	if strings.Contains(logged(), typed) {
		t.Error("the log holds what was typed")
	}
}

// An operation that ends by itself closes the WebSocket with the reason,
// and closing the WebSocket ends the operation at once.
func TestOperateEnds(t *testing.T) {
	a := &opAPI{}
	s := operateServer(t, a, Config{})
	l := first(s)
	tok := signIn(t, s)
	open := func() (*websocket.Conn, *fakeOp) {
		t.Helper()
		n := len(a.started())
		c, _, err := dialOperate(t, l, origin(l))
		if err != nil {
			t.Fatal(err)
		}
		c.WriteJSON(first2(tok))
		if m := readOut(t, c); m.Started == nil {
			t.Fatalf("not started: %+v", m)
		}
		return c, a.started()[n]
	}
	for _, reason := range []string{control.HostIdle, control.HostGone, control.HostOwn, control.HostStopping} {
		c, op := open()
		op.finish(reason)
		if m := readOut(t, c); m.Ended != reason {
			t.Errorf("%s: %+v", reason, m)
		}
		if code, why := closedWith(t, c); code != websocket.CloseNormalClosure || why != reason {
			t.Errorf("%s: closed with %d %q", reason, code, why)
		}
	}
	c, op := open()
	start := time.Now()
	c.Close()
	select {
	case <-op.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing the WebSocket did not end the operation")
	}
	if op.Reason() != control.HostClosed || time.Since(start) > time.Second {
		t.Errorf("ended with %q after %v", op.Reason(), time.Since(start))
	}
	// A close frame of the page's own ends it the same way.
	c, op = open()
	c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "tab closed"))
	select {
	case <-op.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the page's close frame did not end the operation")
	}
}

// A start the core refuses is refused to the page with a reason it can
// show, and the WebSocket closes.
func TestOperateRefusedByTheCore(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("core: OperateHost: not the owner: %w", control.ErrForbidden), "may not type"},
		{fmt.Errorf("core: OperateHost: tmux pane %%1 is being operated by owner since 2026-11-20T10:00:00Z: %w", core.ErrBusy), "operated by owner since"},
		{fmt.Errorf("core: OperateHost: %w", core.ErrNotFound), "not there"},
	} {
		a := &opAPI{refuse: tc.err}
		s := operateServer(t, a, Config{})
		l := first(s)
		c, _, err := dialOperate(t, l, origin(l))
		if err != nil {
			t.Fatal(err)
		}
		c.WriteJSON(first2(signIn(t, s)))
		if m := readOut(t, c); !strings.Contains(m.Refused, tc.want) {
			t.Errorf("%v: %+v", tc.err, m)
		}
		if code, why := closedWith(t, c); code != websocket.ClosePolicyViolation || !strings.Contains(why, tc.want) {
			t.Errorf("%v: closed with %d %q", tc.err, code, why)
		}
	}
}

// testClock is a clock that moves only when the test moves it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// On the web listener a person with no passkey starts an operation only
// within five minutes of the session's sign-in; after that the check
// route and the WebSocket refuse, with a reason the page shows.
func TestOperateOnTheWebWithoutAPasskey(t *testing.T) {
	clock := &testClock{at: time.Now()}
	a := &opAPI{}
	s := operateServer(t, a, Config{Web: &Web{Proxy: "127.0.0.1", URL: "https://dash.example.com"}, Now: clock.now})
	l := first(s)
	tok := signIn(t, s, control.WayWeb)
	check := func() answer {
		t.Helper()
		hr := requestTo(t, l, http.MethodPost, OperateCheckPath, strings.NewReader(`{"pane":"%1"}`))
		hr.Header.Set("Authorization", "Bearer "+tok)
		hr.Header.Set("Origin", origin(l))
		resp, err := client(l).Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(strings.Builder)
		bufio.NewReader(resp.Body).WriteTo(b)
		return answer{resp, b.String()}
	}
	start := func() operateOut {
		t.Helper()
		c, _, err := dialOperate(t, l, origin(l))
		if err != nil {
			t.Fatal(err)
		}
		c.WriteJSON(first2(tok))
		m := readOut(t, c)
		if m.Started != nil {
			c.WriteJSON(map[string]any{"end": true})
			readOut(t, c)
		}
		return m
	}
	clock.add(4 * time.Minute)
	if got := check(); got.StatusCode != http.StatusOK || !strings.Contains(got.body, `"needs":"nothing"`) {
		t.Fatalf("the check within five minutes: %s %s", got.Status, got.body)
	}
	if m := start(); m.Started == nil {
		t.Fatalf("within five minutes: %+v", m)
	}
	clock.add(2 * time.Minute)
	if got := check(); got.StatusCode != http.StatusForbidden || !strings.Contains(got.body, "sign in again") {
		t.Errorf("the check after six minutes: %s %s", got.Status, got.body)
	}
	if m := start(); m.Started != nil || !strings.Contains(m.Refused, "sign in again") {
		t.Errorf("after six minutes: %+v", m)
	}
	if n := len(a.started()); n != 1 {
		t.Errorf("%d operations started", n)
	}
	// Without the page's Origin the check route is refused too.
	hr := requestTo(t, l, http.MethodPost, OperateCheckPath, strings.NewReader(`{"pane":"%1"}`))
	hr.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := client(l).Do(hr); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("the check without an Origin: %v %v", resp, err)
	}
}

// screenMessages reads the pane stream's messages as they come.
func screenMessages(body interface{ Read([]byte) (int, error) }) <-chan hostScreen {
	out := make(chan hostScreen, 1000)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var h hostScreen
				json.Unmarshal([]byte(d), &h)
				out <- h
			}
		}
	}()
	return out
}

// While a pane is operated, its screen stream reads it at least five times
// a second, with the cursor; with no operation it reads once a second, as
// before, and carries no cursor.
func TestScreenStreamWhileOperated(t *testing.T) {
	a := &opAPI{}
	s := operateServer(t, a, Config{})
	l := first(s)
	tok := signIn(t, s)
	hr := requestTo(t, l, http.MethodGet, HostScreenPath+"?"+url.Values{"pane": {"%1"}}.Encode(), nil)
	hr.Header.Set("Authorization", "Bearer "+tok)
	resp, err := client(l).Do(hr)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	defer resp.Body.Close()
	msgs := screenMessages(resp.Body)
	if m := <-msgs; m.Cursor != nil || m.Screen != "a screen" {
		t.Fatalf("not operated: %+v", m)
	}
	count := func(d time.Duration) (screens, views int) {
		s0, v0 := a.reads()
		time.Sleep(d)
		s1, v1 := a.reads()
		return s1 - s0, v1 - v0
	}
	if sc, v := count(2100 * time.Millisecond); sc < 1 || sc > 3 || v != 0 {
		t.Errorf("not operated: %d screen reads and %d with the cursor in 2.1 s", sc, v)
	}

	c, _, err := dialOperate(t, l, origin(l))
	if err != nil {
		t.Fatal(err)
	}
	c.WriteJSON(first2(tok))
	if m := readOut(t, c); m.Started == nil {
		t.Fatalf("not started: %+v", m)
	}
	time.Sleep(1100 * time.Millisecond) // the stream's slow wait ends, the fast reads begin
	if sc, v := count(time.Second); v < 5 || sc != 0 {
		t.Errorf("operated: %d reads with the cursor and %d without in a second", v, sc)
	}
	var withCursor bool
	for drained := false; !drained; {
		select {
		case m := <-msgs:
			withCursor = withCursor || m.Cursor != nil && *m.Cursor == cursor{3, 1}
		default:
			drained = true
		}
	}
	if !withCursor {
		t.Error("no message carried the cursor")
	}
	c.WriteJSON(map[string]any{"end": true})
	readOut(t, c)
	time.Sleep(300 * time.Millisecond)
	if sc, v := count(2100 * time.Millisecond); sc > 3 || v > 1 {
		t.Errorf("after the end: %d screen reads and %d with the cursor in 2.1 s", sc, v)
	}
}
