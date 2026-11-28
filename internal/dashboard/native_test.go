package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/nativehttp"
)

func nativeServer(t *testing.T, api control.API, cfg Config) (*Server, *http.Client) {
	t.Helper()
	cfg.Native = &Native{Transport: nativehttp.Config{Root: t.TempDir(), Instance: "fixture-installation", Version: "fixture-version"}, Person: "owner", API: api}
	if err := os.Chmod(cfg.Native.Transport.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Listen(cfg, only(noAPI{}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if runtime.GOOS == "darwin" && errors.Is(err, nativehttp.ErrACLUnavailable) {
		t.Skip("this Darwin build cannot inspect ACLs; native access correctly refused")
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Serve(context.Background()) }()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nativehttp.Dial(ctx, cfg.Native.Transport)
	}}
	t.Cleanup(func() { s.CloseAndWait(); <-done; tr.CloseIdleConnections() })
	return s, &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func nativeRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, "http://instance"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", NativeOrigin)
	}
	return r
}

func nativeAnswer(t *testing.T, c *http.Client, r *http.Request) answer {
	t.Helper()
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{resp, string(b)}
}

type nativeAPI struct {
	control.API
	mu                         sync.Mutex
	sends                      int
	eventsEnded, terminalEnded chan struct{}
}

func (a *nativeAPI) Threads(context.Context, control.Filter) ([]control.ThreadInfo, error) {
	return []control.ThreadInfo{{ID: 71, Scope: "native-owner"}}, nil
}

func (a *nativeAPI) Send(_ context.Context, id control.ThreadID, key string, in control.Input, _ control.Busy) error {
	if id != 71 || key != "dashboard:native-test" || in.Text != "fixture input" {
		return errors.New("wrong input")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sends++
	return nil
}

func (a *nativeAPI) Events(ctx context.Context, _ control.Filter, after uint64) (<-chan control.Event, error) {
	out := make(chan control.Event)
	go func() {
		defer close(out)
		defer close(a.eventsEnded)
		select {
		case out <- control.Event{Pos: after + 1, Kind: "fixture"}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return out, nil
}

func (a *nativeAPI) Terminal(ctx context.Context, id control.ThreadID, run string, after uint64) (<-chan control.TerminalChunk, error) {
	out := make(chan control.TerminalChunk)
	go func() {
		defer close(out)
		defer close(a.terminalEnded)
		select {
		case out <- control.TerminalChunk{Pos: after + 1, Run: run}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return out, nil
}

func TestNativeAuthorityAndIsolation(t *testing.T) {
	a := &nativeAPI{}
	s, c := nativeServer(t, a, Config{Local: &Local{Listen: "127.0.0.1:0"}, Web: &Web{Listen: "127.0.0.1:0", Proxy: "127.0.0.1", URL: "https://dashboard.example"}, MayAct: func(p string) bool { return p == "owner" }})
	for path, contains := range map[string]string{MePath: `"way":"native"`, NativeIdentityPath: `"instance":"fixture-installation"`, ThreadsPath: `"scope":"native-owner"`} {
		r := nativeAnswer(t, c, nativeRequest(t, "GET", path, ""))
		if r.StatusCode != 200 || !strings.Contains(r.body, contains) || strings.Contains(r.body, `"session"`) {
			t.Fatalf("%s: %d %s", path, r.StatusCode, r.body)
		}
	}
	if r := nativeAnswer(t, c, nativeRequest(t, "GET", MePath, "")); !strings.Contains(r.body, `"person":"owner"`) {
		t.Fatalf("native owner identity: %s", r.body)
	}
	page := nativeAnswer(t, c, nativeRequest(t, "GET", "/", ""))
	asset := regexp.MustCompile(`src="(/assets/[^" ]+\.js)"`).FindStringSubmatch(page.body)
	if page.StatusCode != 200 || len(asset) != 2 {
		t.Fatalf("native page: %d", page.StatusCode)
	}
	js := nativeAnswer(t, c, nativeRequest(t, "GET", asset[1], ""))
	if js.StatusCode != 200 || !strings.Contains(js.body, "api/threads") {
		t.Fatalf("native entry asset: %d", js.StatusCode)
	}
	if _, err := s.NewLink(context.Background(), "owner", NativeWay); err == nil {
		t.Fatal("native sign-in link issued")
	}
	for _, l := range []*listener{s.local, s.web} {
		if r := do(t, s, req{l: l, path: MePath}); r.StatusCode != 401 {
			t.Fatalf("unsigned TCP: %d", r.StatusCode)
		}
		link, err := s.NewLink(context.Background(), "owner", l.way)
		if err != nil {
			t.Fatal(err)
		}
		traded := do(t, s, trade(t, link, req{l: l}))
		var signed signedIn
		if err := json.Unmarshal([]byte(traded.body), &signed); err != nil || signed.Session == "" {
			t.Fatalf("browser trade: %d %s", traded.StatusCode, traded.body)
		}
		token := signed.Session
		if r := do(t, s, req{l: l, path: MePath, token: token}); r.StatusCode != 200 || !strings.Contains(r.body, `"way":"`+string(l.way)+`"`) {
			t.Fatalf("browser sign-in: %d %s", r.StatusCode, r.body)
		}
		r := nativeRequest(t, "GET", MePath, "")
		r.Header.Set("Authorization", "Bearer "+token)
		if got := nativeAnswer(t, c, r); got.StatusCode != 403 {
			t.Fatalf("browser bearer on native: %d", got.StatusCode)
		}
		r2 := do(t, s, req{l: l, path: NativeIdentityPath, token: token})
		if r2.StatusCode != 404 {
			t.Fatalf("native setup on TCP: %d", r2.StatusCode)
		}
		if r := do(t, s, req{l: l, path: MePath, host: "instance", origin: NativeOrigin, header: map[string]string{"X-Hataori-Native": "owner"}}); r.StatusCode != 403 {
			t.Fatalf("native spoof on TCP: %d", r.StatusCode)
		}
	}
	r := nativeAnswer(t, c, nativeRequest(t, "POST", SendPath, `{"thread":71,"key":"native-test","text":"fixture input"}`))
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("native write: %d %s", r.StatusCode, r.body)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sends != 1 {
		t.Fatalf("%d owner writes", a.sends)
	}
}

func TestNativeRefusesHostileHTTP(t *testing.T) {
	a := &nativeAPI{}
	s, c := nativeServer(t, a, Config{MayAct: func(string) bool { return true }})
	s.Handle("/new-owner-route", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "exposed") }))
	cases := []struct {
		name, method, path string
		change             func(*http.Request)
	}{
		{"foreign host", "GET", MePath, func(r *http.Request) { r.Host = "localhost" }},
		{"port host", "GET", MePath, func(r *http.Request) { r.Host = "instance:80" }},
		{"foreign origin", "GET", MePath, func(r *http.Request) { r.Header.Set("Origin", "https://instance") }},
		{"duplicate origin", "POST", SendPath, func(r *http.Request) { r.Header.Add("Origin", "https://evil.example") }},
		{"empty origin", "GET", MePath, func(r *http.Request) { r.Header["Origin"] = []string{""} }},
		{"missing write origin", "POST", SendPath, func(r *http.Request) { r.Header.Del("Origin") }},
		{"missing upgrade origin", "GET", OperatePath, nil},
		{"foreign upgrade origin", "GET", AttachPath, func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{"bearer", "GET", MePath, func(r *http.Request) { r.Header.Set("Authorization", "Bearer owner") }},
		{"empty bearer", "GET", MePath, func(r *http.Request) { r.Header["Authorization"] = []string{""} }},
		{"method", "DELETE", ThreadsPath, nil},
		{"sign-in", "GET", SessionPath, nil}, {"passkey", "GET", "/api/passkeys", nil},
		{"new route", "GET", "/new-owner-route", nil}, {"wire op", "POST", "/api/start", nil},
		{"encoded path", "GET", "/api/%6de", nil}, {"normalized path", "GET", "/api/../api/me", nil},
		{"double slash", "GET", "//api/me", nil},
		{"trailing dot segment", "GET", "/assets/..", nil},
		{"upgrade read", "GET", MePath, func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }},
	}
	for _, h := range append(proxyHeaders, "X-Forwarded-Port") {
		name := h
		cases = append(cases, struct {
			name, method, path string
			change             func(*http.Request)
		}{name, "GET", MePath, func(r *http.Request) { r.Header[name] = []string{""} }})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := nativeRequest(t, tc.method, tc.path, `{"thread":71,"key":"native-test","text":"fixture input"}`)
			if tc.change != nil {
				tc.change(r)
			}
			got := nativeAnswer(t, c, r)
			if got.StatusCode < 400 || got.StatusCode >= 500 {
				t.Fatalf("%d %s", got.StatusCode, got.body)
			}
		})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sends != 0 {
		t.Fatal("hostile request reached the owner API")
	}
}

func TestNativeStreamsAndShutdown(t *testing.T) {
	a := &nativeAPI{eventsEnded: make(chan struct{}), terminalEnded: make(chan struct{})}
	s, c := nativeServer(t, a, Config{})
	for _, tc := range []struct{ path, position, want string }{{EventsPath, "40", "id: 41"}, {TerminalPath + "?thread=71", "20:fixture", "id: 21:fixture"}} {
		r := nativeRequest(t, "GET", tc.path, "")
		r.Header.Set("Last-Event-ID", tc.position)
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("stream %s: %d", tc.path, resp.StatusCode)
		}
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(line, tc.want) {
				break
			}
		}
	}
	s.CloseAndWait()
	for _, ended := range []chan struct{}{a.eventsEnded, a.terminalEnded} {
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatal("native producer outlived shutdown")
		}
	}
}

func TestNativeRejectsHTTPProxyTargets(t *testing.T) {
	s, _ := nativeServer(t, &nativeAPI{}, Config{})
	for _, target := range []string{"http://instance/api/me", "hataori-desktop://instance/api/me"} {
		c, err := nativehttp.Dial(context.Background(), s.native.native.Transport)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		_, err = io.WriteString(c, "GET "+target+" HTTP/1.1\r\nHost: instance\r\nConnection: close\r\n\r\n")
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		r, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		r.Body.Close()
		c.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("proxy target %q: %d", target, r.StatusCode)
		}
	}
}

func nativeWebSocket(t *testing.T, s *Server, path string) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nativehttp.Dial(ctx, s.native.native.Transport)
	}, HandshakeTimeout: time.Second}
	c, _, err := d.Dial("ws://instance"+path, http.Header{"Origin": []string{NativeOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func TestNativeOperateAuthenticationAndShutdown(t *testing.T) {
	a := &opAPI{}
	s, _ := nativeServer(t, a, Config{HostInput: true})
	for _, token := range []string{"forged", "browser-bearer"} {
		c := nativeWebSocket(t, s, OperatePath)
		c.WriteJSON(operateFirst{Session: token, Pane: "%1"})
		if got := readOut(t, c); got.Refused != "not signed in" {
			t.Fatalf("native bearer accepted: %+v", got)
		}
	}
	c := nativeWebSocket(t, s, OperatePath)
	c.WriteJSON(operateFirst{Pane: "%1"})
	if got := readOut(t, c); got.Started == nil {
		t.Fatalf("empty native session: %+v", got)
	}
	c.WriteJSON(operateIn{Keys: []control.HostKey{{Key: "Up"}}})
	c.WriteJSON(operateIn{End: true})
	if got := readOut(t, c); got.Ended != control.HostEnded {
		t.Fatalf("native operation: %+v", got)
	}
	if got := a.started()[0].batches(); len(got) != 1 || got[0][0].Key != "Up" {
		t.Fatalf("keys: %+v", got)
	}
	c = nativeWebSocket(t, s, OperatePath)
	c.WriteJSON(operateFirst{Pane: "%1"})
	readOut(t, c)
	// A hijacked connection still awaiting its first message must end too.
	nativeWebSocket(t, s, OperatePath)
	s.CloseAndWait()
	select {
	case <-a.started()[1].Done():
	case <-time.After(time.Second):
		t.Fatal("operation outlived shutdown")
	}
}

func TestNativeWebSocketOrigin(t *testing.T) {
	s, _ := nativeServer(t, &opAPI{}, Config{HostInput: true})
	for _, route := range []string{OperatePath, AttachPath} {
		for _, origin := range []string{"", "null", "https://instance"} {
			d := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return nativehttp.Dial(ctx, s.native.native.Transport)
			}, HandshakeTimeout: time.Second}
			h := http.Header{}
			if origin != "" {
				h.Set("Origin", origin)
			}
			c, resp, err := d.Dial("ws://instance"+route, h)
			if c != nil {
				c.Close()
			}
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s origin %q: %v %v", route, origin, resp, err)
			}
		}
	}
}

func TestNativePreauthShutdown(t *testing.T) {
	s, _ := nativeServer(t, &opAPI{}, Config{HostInput: true})
	c := nativeWebSocket(t, s, OperatePath)
	defer c.Close()
	done := make(chan struct{})
	go func() { s.CloseAndWait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("shutdown did not close a WebSocket awaiting its first message")
		c.Close()
		<-done
	}
}

type nativeBlockedAPI struct {
	control.API
	entered, canceled, release chan struct{}
}

func (a nativeBlockedAPI) Threads(ctx context.Context, _ control.Filter) ([]control.ThreadInfo, error) {
	close(a.entered)
	<-ctx.Done()
	close(a.canceled)
	<-a.release
	return nil, ctx.Err()
}

func TestNativeShutdownWaitsForAPI(t *testing.T) {
	a := nativeBlockedAPI{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	s, c := nativeServer(t, a, Config{})
	var once sync.Once
	defer once.Do(func() { close(a.release) })
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, _ := c.Do(nativeRequest(t, "GET", ThreadsPath, ""))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("API not entered")
	}
	stopped := make(chan struct{})
	go func() { s.CloseAndWait(); close(stopped) }()
	select {
	case <-a.canceled:
	case <-time.After(time.Second):
		t.Fatal("API context not canceled")
	}
	select {
	case <-stopped:
		t.Fatal("shutdown returned while API still used dependencies")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(a.release) })
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join request")
	}
	<-requestDone
}

type nativeHeldProducer struct {
	control.API
	canceled, release chan struct{}
}

func (a nativeHeldProducer) Events(ctx context.Context, _ control.Filter, _ uint64) (<-chan control.Event, error) {
	out := make(chan control.Event)
	go func() { defer close(out); <-ctx.Done(); close(a.canceled); <-a.release }()
	return out, nil
}

func TestNativeShutdownJoinsStreamProducer(t *testing.T) {
	for _, mode := range []string{"shutdown", "head", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			a := nativeHeldProducer{canceled: make(chan struct{}), release: make(chan struct{})}
			s, c := nativeServer(t, a, Config{})
			var once sync.Once
			defer once.Do(func() { close(a.release) })
			if mode == "head" {
				go func() {
					resp, _ := c.Do(nativeRequest(t, "HEAD", EventsPath, ""))
					if resp != nil {
						resp.Body.Close()
					}
				}()
			} else {
				resp, err := c.Do(nativeRequest(t, "GET", EventsPath, ""))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if mode == "disconnect" {
					resp.Body.Close()
				}
			}
			if mode == "shutdown" {
				s.Close()
			} // cancel while the producer still owns its dependencies
			select {
			case <-a.canceled:
			case <-time.After(time.Second):
				t.Fatal("producer did not see cancellation")
			}
			done := make(chan struct{})
			go func() { s.CloseAndWait(); close(done) }()
			select {
			case <-done:
				t.Error("shutdown returned before the producer finished")
			case <-time.After(20 * time.Millisecond):
			}
			once.Do(func() { close(a.release) })
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("shutdown did not join released producer")
			}
		})
	}
}

type nativeHeldScreen struct {
	control.API
	mu                         sync.Mutex
	calls                      int
	entered, canceled, release chan struct{}
}

func (a *nativeHeldScreen) HostScreen(ctx context.Context, _ string) (string, error) {
	a.mu.Lock()
	a.calls++
	first := a.calls == 1
	a.mu.Unlock()
	if first {
		return "fixture screen", nil
	}
	close(a.entered)
	<-ctx.Done()
	close(a.canceled)
	<-a.release
	return "", ctx.Err()
}

func TestNativeShutdownJoinsHostScreen(t *testing.T) {
	a := &nativeHeldScreen{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	s, c := nativeServer(t, a, Config{})
	var once sync.Once
	defer once.Do(func() { close(a.release) })
	resp, err := c.Do(nativeRequest(t, "GET", HostScreenPath+"?pane=%251", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-a.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("host screen producer did not enter API")
	}
	done := make(chan struct{})
	go func() { s.CloseAndWait(); close(done) }()
	select {
	case <-a.canceled:
	case <-time.After(time.Second):
		t.Fatal("host screen API not canceled")
	}
	select {
	case <-done:
		t.Error("shutdown returned before host screen API completed")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(a.release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join host screen")
	}
}

func TestNativeOnlyDoesNotBindTCPOrIssueLinks(t *testing.T) {
	s, _ := nativeServer(t, &nativeAPI{}, Config{Bind: func(string, string) (net.Listener, error) {
		t.Error("native attempted TCP bind")
		return nil, errors.New("TCP forbidden")
	}})
	if s.local != nil || s.web != nil {
		t.Fatal("browser listener created implicitly")
	}
	if _, err := s.NewLink(context.Background(), "owner", ""); err == nil {
		t.Fatal("native-only issued browser credential")
	}
	if _, err := os.Stat(s.native.native.Transport.Root + "/state/" + nativehttp.DiscoveryFile); err != nil {
		t.Fatal(err)
	}
}

type nativeAttachAPI struct{ control.API }

func (nativeAttachAPI) AttachTerminal(context.Context, control.ThreadID) (control.Attachment, error) {
	return control.Attachment{Socket: "fixture-host"}, nil
}

type nativeTerminal struct {
	mu         sync.Mutex
	input      string
	cols, rows int
	closed     chan struct{}
	once       sync.Once
}

func (t *nativeTerminal) Input(b []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.input += string(b)
	return nil
}
func (t *nativeTerminal) Resize(cols, rows int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols, t.rows = cols, rows
	return nil
}
func (t *nativeTerminal) Wait() error  { <-t.closed; return nil }
func (t *nativeTerminal) Close() error { t.once.Do(func() { close(t.closed) }); return nil }

func TestNativeAttachAuthenticationAndShutdown(t *testing.T) {
	term := &nativeTerminal{closed: make(chan struct{})}
	s, _ := nativeServer(t, nativeAttachAPI{}, Config{AttachHost: func(socket string) (TerminalInput, error) {
		if socket != "fixture-host" {
			return nil, errors.New("wrong host")
		}
		return term, nil
	}})
	c := nativeWebSocket(t, s, AttachPath)
	c.WriteJSON(attachFirst{Session: "browser-bearer", Thread: 71})
	var out attachOut
	if err := c.ReadJSON(&out); err != nil || out.Refused != "not signed in" {
		t.Fatalf("native attach bearer: %+v %v", out, err)
	}
	c = nativeWebSocket(t, s, AttachPath)
	c.WriteJSON(attachFirst{Thread: 71})
	out = attachOut{}
	if err := c.ReadJSON(&out); err != nil || out.Attached == nil {
		t.Fatalf("native attach start: %+v %v", out, err)
	}
	c.WriteJSON(attachIn{Keys: "fixture keys"})
	c.WriteJSON(attachIn{Resize: &attachSize{80, 24}})
	c.WriteJSON(attachIn{End: true})
	out = attachOut{}
	if err := c.ReadJSON(&out); err != nil || out.Ended != "ended" {
		t.Fatalf("native attach end: %+v %v", out, err)
	}
	s.CloseAndWait()
	term.mu.Lock()
	defer term.mu.Unlock()
	if term.input != "fixture keys" || term.cols != 80 || term.rows != 24 {
		t.Fatalf("terminal input %q size %dx%d", term.input, term.cols, term.rows)
	}
	select {
	case <-term.closed:
	default:
		t.Fatal("host terminal outlived shutdown")
	}
}
