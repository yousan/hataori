package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/wire"
)

// attachMsg is a message of the attachment WebSocket to the page.
type attachMsg struct {
	Attached *struct{ Idle int }
	Error    string
	Ended    string
	Refused  string
}

// dialAttach opens the attachment WebSocket on the loopback listener at
// base, with the page's Origin unless origin is false.
func dialAttach(t *testing.T, base string, origin bool) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, _ := url.Parse(base)
	h := http.Header{}
	if origin {
		h.Set("Origin", base)
	}
	c, resp, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).Dial("ws://"+u.Host+dashboard.AttachPath, h)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, resp, err
}

// startAttach opens an attachment of thread id with session s and returns
// it with the server's first answer.
func startAttach(t *testing.T, base string, s dashSession, id control.ThreadID) (*websocket.Conn, attachMsg) {
	t.Helper()
	c, resp, err := dialAttach(t, base, true)
	if err != nil {
		t.Fatalf("the attachment WebSocket: %v %v", resp, err)
	}
	c.WriteJSON(map[string]any{"session": s, "thread": id})
	return c, readAttach(t, c)
}

func readAttach(t *testing.T, c *websocket.Conn) attachMsg {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(100 * time.Second))
	var m attachMsg
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("reading the attachment: %v", err)
	}
	return m
}

// attachRoot is a chat root with the fake agent in a terminal, the
// dashboard on loopback, sessions put to sleep after idle and attachments
// ended after attachIdle without a key.
func attachRoot(t *testing.T, idle, attachIdle string) *chatRoot {
	t.Helper()
	conv := filepath.Join(t.TempDir(), "conversations")
	os.MkdirAll(conv, 0o700)
	return newChatRoot(t, func(cfg map[string]any) {
		cfg["idle"], cfg["host_input_idle"] = idle, attachIdle
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv, Terminal: true, Hataori: os.Args[0]}}
	})
}

// threadOf is the thread linked to conv, as the owner reads it.
func threadOf(t *testing.T, root string) control.ThreadInfo {
	t.Helper()
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ts, err := c.Threads(context.Background(), control.Filter{})
	if err != nil || len(ts) != 1 {
		t.Fatalf("threads: %v %v", ts, err)
	}
	return ts[0]
}

// inNoRow fails when any row of any table of root's database holds marker.
func inNoRow(t *testing.T, root, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, store.File)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tb := range tables {
		rs, err := db.Query(`SELECT * FROM "` + tb + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rs.Columns()
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rs.Scan(ptrs...)
			for _, v := range vals {
				if b, ok := v.([]byte); ok && strings.Contains(string(b), marker) || fmt.Sprint(v) != "" && strings.Contains(fmt.Sprint(v), marker) {
					rs.Close()
					t.Fatalf("table %s holds what was typed", tb)
				}
			}
		}
		rs.Close()
	}
}

// The acceptance criteria of #607 on the loopback listener, with the fake
// agent in a terminal: keys sent on the attachment reach the session's
// terminal in order, and a line typed there is a turn of the terminal
// whose answer chat posts after "(typed at the terminal)"; while attached
// the session is not put to sleep, and its terminal keeps its size; the
// page ending the attachment ends it, after which the idle period puts the
// session to sleep; a start and an end are one log line each, and a marker
// typed and erased is in no log line and no row; attaching to the asleep
// thread wakes it.
func TestDashboardAttachTypesIntoTheSession(t *testing.T) {
	r := attachRoot(t, "2s", "1h")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen on-the-screen")
	r.posted(t, th, "done")
	x := threadOf(t, r.root)
	eventually(t, "the session's terminal", 15*time.Second, func() bool { x = threadOf(t, r.root); return x.Terminal != "" })
	size := ptySize(t, x.Terminal)
	base, s := dashSignIn(t, r.root)

	c, m := startAttach(t, base, s, x.ID)
	if m.Attached == nil || m.Attached.Idle != 3600 {
		t.Fatalf("the start: %+v", m)
	}
	// Attached for longer than the idle period: not put to sleep.
	time.Sleep(3 * time.Second)
	if x = threadOf(t, r.root); x.State != control.Idle {
		t.Fatalf("attached past the idle period: %s", x.State)
	}
	if got := ptySize(t, x.Terminal); got != size {
		t.Fatalf("the terminal is %s while attached, %s before", got, size)
	}
	// A marker typed and erased, then a line in two batches.
	const marker = "MARKERzq9"
	c.WriteJSON(map[string]any{"keys": marker + strings.Repeat("\x7f", len(marker))})
	c.WriteJSON(map[string]any{"keys": "reply typed "})
	c.WriteJSON(map[string]any{"keys": "here\r"})
	r.posted(t, th, "(typed at the terminal)\ntyped here")
	c.WriteJSON(map[string]any{"end": true})
	if m := readAttach(t, c); m.Ended != "ended" {
		t.Fatalf("the end: %+v", m)
	}
	eventually(t, "the end logged", 5*time.Second, func() bool {
		return strings.Count(logged(), `msg="dashboard: terminal detached"`) == 1
	})
	if n := strings.Count(logged(), `msg="dashboard: terminal attached"`); n != 1 || !strings.Contains(logged(), `reason=ended`) {
		t.Fatalf("%d starts logged, and the end's reason: %s", n, logged())
	}
	if strings.Contains(logged(), marker) {
		t.Fatal("the log holds what was typed")
	}
	inNoRow(t, r.root, marker)
	if got := ptySize(t, x.Terminal); got != size {
		t.Fatalf("the terminal is %s after the attachment, %s before", got, size)
	}
	// Detached, the idle period puts it to sleep.
	eventually(t, "the session asleep", 20*time.Second, func() bool { return threadOf(t, r.root).State == control.Asleep })

	// Attaching to the asleep thread wakes it, with no turn.
	c, m = startAttach(t, base, s, x.ID)
	if m.Attached == nil {
		t.Fatalf("attaching to the asleep thread: %+v", m)
	}
	eventually(t, "the woken thread idle", 20*time.Second, func() bool { return threadOf(t, r.root).State == control.Idle })
	c.WriteJSON(map[string]any{"keys": "reply after the wake\r"})
	r.posted(t, th, "(typed at the terminal)\nafter the wake")
}

// An attachment ends after the idle period without a key, and the page is
// told so; each key starts the period again; one whose page goes away ends
// at once.
func TestDashboardAttachEndsWhenIdle(t *testing.T) {
	r := attachRoot(t, "1h", "1s")
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	x := threadOf(t, r.root)
	base, s := dashSignIn(t, r.root)
	c, m := startAttach(t, base, s, x.ID)
	if m.Attached == nil || m.Attached.Idle != 1 {
		t.Fatalf("the start: %+v", m)
	}
	at := time.Now()
	if m := readAttach(t, c); m.Ended != "idle" || time.Since(at) > 5*time.Second {
		t.Fatalf("after the idle period: %+v after %v", m, time.Since(at))
	}
	// Keys start the idle period again: typed every 400 ms for 2.4 s, the
	// attachment lasts, and ends about a second after the last key.
	c3, m := startAttach(t, base, s, x.ID)
	if m.Attached == nil {
		t.Fatalf("the third start: %+v", m)
	}
	ended := make(chan attachMsg, 1)
	endedAt := make(chan time.Time, 1)
	go func() {
		var m attachMsg
		c3.SetReadDeadline(time.Now().Add(30 * time.Second))
		c3.ReadJSON(&m)
		endedAt <- time.Now()
		ended <- m
	}()
	for range 6 {
		c3.WriteJSON(map[string]any{"keys": "x"})
		time.Sleep(400 * time.Millisecond)
	}
	last := time.Now()
	if at, m := <-endedAt, <-ended; m.Ended != "idle" || at.Before(last.Add(-400*time.Millisecond)) {
		t.Fatalf("with keys every 400 ms: %+v at %v, the last key at %v", m, at, last)
	}
	c2, m := startAttach(t, base, s, x.ID)
	if m.Attached == nil {
		t.Fatalf("the second start: %+v", m)
	}
	c2.Close()
	eventually(t, "the closed page's end logged", 5*time.Second, func() bool { return strings.Contains(logged(), `reason=closed`) })
}

// Refused: a person who is not the owner, a thread whose agent runs on
// pipes, a closed thread, and a handshake without the page's Origin.
func TestDashboardAttachRefusals(t *testing.T) {
	// A thread on pipes.
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply hello"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread idle", 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
	base, s := dashSignIn(t, root)
	if _, m := startAttach(t, base, s, id); m.Attached != nil || !strings.Contains(m.Refused, "no terminal") {
		t.Errorf("a thread on pipes: %+v", m)
	}
	if _, resp, err := dialAttach(t, base, false); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a handshake without Origin: %v %v", resp, err)
	}
	// Another person, whose Caller the daemon gives no OpAttach.
	dashboardHook = func(cl *control.Caller) { cl.Principal, cl.May = "grace", control.OpRead }
	if _, m := startAttach(t, base, s, id); m.Attached != nil || !strings.Contains(m.Refused, "only the owner") {
		t.Errorf("another person: %+v", m)
	}
	dashboardHook = nil
	// A closed thread.
	if code, _, errOut := cli(t, "close", "--root", root, fmt.Sprint(int64(id))); code != 0 {
		t.Fatalf("hataori close: %d %s", code, errOut)
	}
	if _, m := startAttach(t, base, s, id); m.Attached != nil || !strings.Contains(m.Refused, "closed") {
		t.Errorf("a closed thread: %+v", m)
	}
}

// On the web listener an attachment starts only after the check that
// acting needs; on loopback it starts at once (above). The daemon stopping
// ends it, and the page is told.
func TestDashboardAttachOnTheWebNeedsTheCheck(t *testing.T) {
	d := newPasskeyDash(t)
	conv := filepath.Join(t.TempDir(), "conversations")
	os.MkdirAll(conv, 0o700)
	setConfig(t, d.root, func(cfg map[string]any) {
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv, Terminal: true, Hataori: os.Args[0]}}
	})
	stop := runDaemon(t, d.root)
	c, err := wire.Dial(d.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply hello"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the thread idle", 20*time.Second, func() bool { x, err := c.Thread(ctx, id, 0, 0); return err == nil && x.State == control.Idle })
	k := &softKey{rpID: webName, origin: d.web}
	s, reg := d.webLink()
	code, body, cred := d.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("registering a passkey: %d %s", code, body)
	}
	dial := func() (*websocket.Conn, attachMsg) {
		t.Helper()
		ws, resp, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second, TLSClientConfig: d.hc.Transport.(*http.Transport).TLSClientConfig,
			NetDialContext: d.hc.Transport.(*http.Transport).DialContext}).Dial("wss://"+strings.TrimPrefix(d.web, "https://")+dashboard.AttachPath, http.Header{"Origin": {d.web}})
		if err != nil {
			t.Fatalf("the attachment WebSocket on the web: %v %v", resp, err)
		}
		t.Cleanup(func() { ws.Close() })
		ws.WriteJSON(map[string]any{"session": s, "thread": id})
		return ws, readAttach(t, ws)
	}
	if _, m := dial(); m.Attached != nil || !strings.Contains(m.Refused, "passkey check") {
		t.Fatalf("without a check: %+v", m)
	}
	code, b := d.post(dashboard.ActCheckPath, s, map[string]any{})
	var a struct {
		Needs string
		ceremonyAnswer
	}
	json.Unmarshal(b, &a)
	if code != http.StatusOK || a.Needs != "passkey" {
		t.Fatalf("act/check: %d %s", code, b)
	}
	if code, b := d.post(dashboard.ActVerifyPath, s, map[string]any{"ceremony": a.Ceremony, "credential": k.get(t, a.Options, cred)}); code != http.StatusOK {
		t.Fatalf("act/verify: %d %s", code, b)
	}
	ws, m := dial()
	if m.Attached == nil {
		t.Fatalf("after the check: %+v", m)
	}
	stop()
	if m := readAttach(t, ws); m.Ended != "stopping" {
		t.Fatalf("the daemon stopping: %+v", m)
	}
}
