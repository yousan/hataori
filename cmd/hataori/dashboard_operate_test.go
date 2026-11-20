package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/hostterm"
)

// operateMarker is typed in these tests; it must reach no log.
const operateMarker = "hataori-dash-typed-marker-77e1"

// operateRig is a daemon with both dashboard listeners and host input on,
// whose host terminals are a tmux server of the test's own, with one pane
// that keeps what it reads in a file.
type operateRig struct {
	*passkeyDash
	tmux   func(args ...string) string
	pane   string
	device string // the pane's terminal device
	got    string // what the pane read
	logged func() string
}

func newOperateRig(t *testing.T, idle string) *operateRig {
	t.Helper()
	return newOperateRigWith(t, idle, true)
}

// newOperateRigWith is newOperateRig with host_input set to input. The
// pane shows a prompt first, which puts its cursor after it.
func newOperateRigWith(t *testing.T, idle string, input bool) *operateRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no tmux")
		}
		t.Skip("no tmux")
	}
	tmpdir, err := os.MkdirTemp("/tmp", "ho") // short on macOS too: a socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", tmpdir) // the server's socket is in a directory of the test's own
	sock := []string{"-L", fmt.Sprintf("hataori-dash-operate-test-%d", os.Getpid()), "-f", "/dev/null"}
	r := &operateRig{passkeyDash: newPasskeyDash(t), got: filepath.Join(t.TempDir(), "got")}
	r.tmux = func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", slices.Concat(sock, args)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Typed only once the pane's terminal is in raw mode: before stty,
	// Enter's \r would be read as \n (#514).
	ready := filepath.Join(t.TempDir(), "ready")
	r.tmux("new-session", "-d", "-s", "work", "-x", "100", "-y", "30", "sh", "-c", "printf 'ready> '; stty raw -echo && : > "+ready+"; exec cat > "+r.got)
	t.Cleanup(func() { exec.Command("tmux", slices.Concat(sock, []string{"kill-server"})...).Run() })
	eventually(t, "the pane's terminal in raw mode", 10*time.Second, func() bool { _, err := os.Stat(ready); return err == nil })
	r.pane = r.tmux("display-message", "-p", "-t", "work:0", "#{pane_id}")
	r.device = r.tmux("display-message", "-p", "-t", "work:0", "#{pane_tty}")
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = sock }
	t.Cleanup(func() { hostTermsHook = nil })
	setConfig(t, r.root, func(cfg map[string]any) {
		cfg["host_terminals"], cfg["host_input"], cfg["host_input_idle"] = true, input, idle
	})
	r.logged = startDaemonLog(t, r.root)
	return r
}

// localBase is the loopback listener's address, as its links name it.
func (r *operateRig) localBase(t *testing.T) string {
	u, _ := url.Parse(signinLink(t, r.root))
	return u.Scheme + "://" + u.Host
}

// localSession signs in on the loopback listener.
func (r *operateRig) localSession(t *testing.T) dashSession {
	t.Helper()
	s, _ := r.trade(signinLink(t, r.root))
	return s
}

// dial opens the operate WebSocket on base (the web listener's when it is
// https) with the page's Origin.
func (r *operateRig) dial(t *testing.T, base string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(base)
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws := "ws://" + u.Host
	if u.Scheme == "https" {
		ws = "wss://" + u.Host
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		d.NetDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, r.listen)
		}
	}
	c, resp, err := d.Dial(ws+dashboard.OperatePath, http.Header{"Origin": {base}})
	if err != nil {
		t.Fatalf("the operate WebSocket on %s: %v %v", base, resp, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type opMessage struct {
	Started *struct{ Idle int }
	Error   string
	Ended   string
	Refused string
}

func readOp(t *testing.T, c *websocket.Conn) opMessage {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var m opMessage
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("no message: %v", err)
	}
	return m
}

func closeOf(t *testing.T, c *websocket.Conn) (int, string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
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

// typeAndEnd types the marker and Enter, waits until the pane has read
// them, and ends the operation.
func (r *operateRig) typeAndEnd(t *testing.T, c *websocket.Conn) {
	t.Helper()
	c.WriteJSON(map[string]any{"keys": []map[string]string{{"text": operateMarker}, {"key": "Enter"}}})
	eventually(t, "the pane to read what was typed", 10*time.Second, func() bool {
		b, _ := os.ReadFile(r.got)
		return strings.HasSuffix(string(b), operateMarker+"\r")
	})
	c.WriteJSON(map[string]any{"end": true})
	if m := readOp(t, c); m.Ended != "ended" {
		t.Fatalf("after the end: %+v", m)
	}
	os.WriteFile(r.got, nil, 0o600)
}

// noMarkerLogged fails when the daemon logged what was typed.
func (r *operateRig) noMarkerLogged(t *testing.T) {
	t.Helper()
	if strings.Contains(r.logged(), operateMarker) {
		t.Error("the daemon's log holds what was typed")
	}
}

// On the loopback listener an operation starts at once, and what is typed
// reaches the pane. Closing the WebSocket ends the operation at once, and
// its log line says so; an operation that ends by itself, after the idle
// period, closes the WebSocket with the reason.
func TestDashboardOperateOnTheLoopbackListener(t *testing.T) {
	r := newOperateRig(t, "2s")
	base := r.localBase(t)
	s := r.localSession(t)
	c := r.dial(t, base)
	c.WriteJSON(map[string]any{"session": s, "pane": r.pane})
	if m := readOp(t, c); m.Started == nil || m.Started.Idle != 2 {
		t.Fatalf("not started at once: %+v", m)
	}
	r.typeAndEnd(t, c)
	if code, why := closeOf(t, c); code != websocket.CloseNormalClosure || why != "ended" {
		t.Errorf("closed with %d %q", code, why)
	}

	c = r.dial(t, base)
	c.WriteJSON(map[string]any{"session": s, "pane": r.pane})
	if m := readOp(t, c); m.Started == nil {
		t.Fatalf("not started: %+v", m)
	}
	c.Close()
	eventually(t, "the end's log line", 2*time.Second, func() bool {
		return strings.Count(r.logged(), `msg="host input ended"`) == 2 && strings.Contains(r.logged(), "reason=closed")
	})

	c = r.dial(t, base)
	c.WriteJSON(map[string]any{"session": s, "pane": r.pane})
	if m := readOp(t, c); m.Started == nil {
		t.Fatalf("not started: %+v", m)
	}
	if m := readOp(t, c); m.Ended != "idle" {
		t.Errorf("after the idle period: %+v", m)
	}
	if code, why := closeOf(t, c); code != websocket.CloseNormalClosure || why != "idle" {
		t.Errorf("closed with %d %q", code, why)
	}

	// The pane goes: the WebSocket closes with that reason.
	c = r.dial(t, base)
	c.WriteJSON(map[string]any{"session": s, "pane": r.pane})
	if m := readOp(t, c); m.Started == nil {
		t.Fatalf("not started: %+v", m)
	}
	r.tmux("new-window", "-d", "-t", "work:", "exec sleep 600") // the server stays
	r.tmux("kill-pane", "-t", r.pane)
	if m := readOp(t, c); m.Ended != "pane gone" {
		t.Errorf("after the pane went: %+v", m)
	}
	if code, why := closeOf(t, c); code != websocket.CloseNormalClosure || why != "pane gone" {
		t.Errorf("closed with %d %q", code, why)
	}
	r.noMarkerLogged(t)
}

// On the web listener an operation starts only after a passkey check with
// user verification made for this start: not without one, not with one
// made for another pane or used already, not with one that did not verify
// the user, and a check made to start cannot sign anyone in. A person with
// a passkey needs the check even right after signing in.
func TestDashboardOperateOnTheWebNeedsAPasskeyCheck(t *testing.T) {
	r := newOperateRig(t, "5m")
	k := &softKey{rpID: webName, origin: r.web}
	s, reg := r.webLink()
	code, body, cred := r.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("registering a passkey: %d %s", code, body)
	}
	check := func(pane string) ceremonyAnswer {
		t.Helper()
		code, b := r.post(dashboard.OperateCheckPath, s, map[string]any{"pane": pane})
		var a struct {
			Needs string
			ceremonyAnswer
		}
		json.Unmarshal(b, &a)
		if code != http.StatusOK || a.Needs != "passkey" || a.Ceremony == "" {
			t.Fatalf("the check: %d %s", code, b)
		}
		return a.ceremonyAnswer
	}
	start := func(first map[string]any) (*websocket.Conn, opMessage) {
		t.Helper()
		c := r.dial(t, r.web)
		c.WriteJSON(first)
		return c, readOp(t, c)
	}

	// Without a check, right after the sign-in.
	if _, m := start(map[string]any{"session": s, "pane": r.pane}); m.Started != nil || !strings.Contains(m.Refused, "needs a passkey check made for it") {
		t.Errorf("without a check: %+v", m)
	}
	// A check made for another pane.
	a := check("%999")
	if _, m := start(map[string]any{"session": s, "pane": r.pane, "ceremony": a.Ceremony, "credential": k.get(t, a.Options, cred)}); m.Started != nil || !strings.Contains(m.Refused, "made for another start") {
		t.Errorf("a check made for another pane: %+v", m)
	}
	// A check that did not verify the user.
	a = check(r.pane)
	k.noUV = true
	if _, m := start(map[string]any{"session": s, "pane": r.pane, "ceremony": a.Ceremony, "credential": k.get(t, a.Options, cred)}); m.Started != nil || !strings.Contains(m.Refused, "not accepted") {
		t.Errorf("a check without user verification: %+v", m)
	}
	k.noUV = false
	// A check made to start does not finish a sign-in check.
	a = check(r.pane)
	if code, b := r.post(dashboard.PasskeySignInFinishPath, s, map[string]any{"ceremony": a.Ceremony, "credential": k.get(t, a.Options, cred)}); code != http.StatusUnauthorized || !strings.Contains(string(b), "another sign-in") {
		t.Errorf("a check made to operate signed in: %d %s", code, b)
	}

	// A check made for this start: the operation starts, and the check is
	// used up.
	a = check(r.pane)
	answer := k.get(t, a.Options, cred)
	c, m := start(map[string]any{"session": s, "pane": r.pane, "ceremony": a.Ceremony, "credential": answer})
	if m.Started == nil {
		t.Fatalf("with a check made for it: %+v", m)
	}
	r.typeAndEnd(t, c)
	if _, m := start(map[string]any{"session": s, "pane": r.pane, "ceremony": a.Ceremony, "credential": answer}); m.Started != nil || !strings.Contains(m.Refused, "was used") {
		t.Errorf("the same check again: %+v", m)
	}
	if n := strings.Count(r.logged(), `msg="host input started"`); n != 1 {
		t.Errorf("%d operations started", n)
	}
	r.noMarkerLogged(t)
}

// On the web listener a person with no passkey starts an operation within
// five minutes of the session's sign-in, and is refused after, with a
// reason the page shows.
func TestDashboardOperateOnTheWebWithoutAPasskey(t *testing.T) {
	clock := useClock(t)
	r := newOperateRig(t, "5m")
	clock.add(2 * time.Second) // a sign-in in the second the server started counts as before it
	s, _ := r.webLink()
	if code, b := r.post(dashboard.OperateCheckPath, s, map[string]any{"pane": r.pane}); code != http.StatusOK || !strings.Contains(string(b), `"needs":"nothing"`) {
		t.Fatalf("the check right after the sign-in: %d %s", code, b)
	}
	c := r.dial(t, r.web)
	c.WriteJSON(map[string]any{"session": s, "pane": r.pane})
	if m := readOp(t, c); m.Started == nil {
		t.Fatalf("right after the sign-in: %+v", m)
	}
	r.typeAndEnd(t, c)
	clock.add(5*time.Minute + time.Second)
	if code, b := r.post(dashboard.OperateCheckPath, s, map[string]any{"pane": r.pane}); code != http.StatusForbidden || !strings.Contains(string(b), "sign in again") {
		t.Errorf("the check after five minutes: %d %s", code, b)
	}
	c = r.dial(t, r.web)
	c.WriteJSON(map[string]any{"session": s, "pane": r.pane})
	if m := readOp(t, c); m.Started != nil || !strings.Contains(m.Refused, "sign in again") {
		t.Errorf("after five minutes: %+v", m)
	}
	r.noMarkerLogged(t)
}

// A check is bound to the session that asked for it, and a check that
// signs in is not one to operate. A session cannot remove its person's
// passkeys and then do without a check; a fresh link after the removal
// can, as for any person with no passkey.
func TestDashboardOperateOnTheWebCannotGoAroundTheCheck(t *testing.T) {
	clock := useClock(t)
	r := newOperateRig(t, "5m")
	clock.add(2 * time.Second) // a sign-in in the second the server started counts as before it
	k := &softKey{rpID: webName, origin: r.web}
	s, reg := r.webLink()
	code, body, cred := r.register(k, s, reg)
	if code != http.StatusOK {
		t.Fatalf("registering a passkey: %d %s", code, body)
	}
	other, _ := r.webLink()
	start := func(first map[string]any) opMessage {
		t.Helper()
		c := r.dial(t, r.web)
		c.WriteJSON(first)
		m := readOp(t, c)
		if m.Started != nil {
			c.WriteJSON(map[string]any{"end": true})
			readOp(t, c)
		}
		return m
	}

	// A check another session of the same person asked for.
	code, b := r.post(dashboard.OperateCheckPath, other, map[string]any{"pane": r.pane})
	var a struct {
		Needs string
		ceremonyAnswer
	}
	json.Unmarshal(b, &a)
	if code != http.StatusOK || a.Needs != "passkey" {
		t.Fatalf("the check: %d %s", code, b)
	}
	if m := start(map[string]any{"session": s, "pane": r.pane, "ceremony": a.Ceremony, "credential": k.get(t, a.Options, cred)}); m.Started != nil || !strings.Contains(m.Refused, "made for another start") {
		t.Errorf("a check of another session: %+v", m)
	}
	// A check that signs in.
	code, b = r.post(dashboard.PasskeySignInBeginPath, s, map[string]any{"check": true})
	var in ceremonyAnswer
	json.Unmarshal(b, &in)
	if code != http.StatusOK || in.Ceremony == "" {
		t.Fatalf("a sign-in check: %d %s", code, b)
	}
	if m := start(map[string]any{"session": s, "pane": r.pane, "ceremony": in.Ceremony, "credential": k.get(t, in.Options, cred)}); m.Started != nil || !strings.Contains(m.Refused, "made for another start") {
		t.Errorf("a sign-in check used to operate: %+v", m)
	}

	// The session removes the passkey, then tries without a check.
	if code, b := r.do(http.MethodDelete, "", dashboard.PasskeysPath+"/"+b64(cred.id), s, r.web, nil); code != http.StatusNoContent {
		t.Fatalf("removing the passkey: %d %s", code, b)
	}
	if m := start(map[string]any{"session": s, "pane": r.pane}); m.Started != nil || !strings.Contains(m.Refused, "sign in again") {
		t.Errorf("after the session removed its person's passkey: %+v", m)
	}
	if code, _ := r.post(dashboard.OperateCheckPath, s, map[string]any{"pane": r.pane}); code != http.StatusForbidden {
		t.Errorf("the check after the removal: %d", code)
	}
	// A fresh link after the removal: a person with no passkey.
	clock.add(2 * time.Second)
	fresh, _ := r.webLink()
	if m := start(map[string]any{"session": fresh, "pane": r.pane}); m.Started == nil {
		t.Errorf("a link signed in after the removal: %+v", m)
	}
	r.noMarkerLogged(t)
}

// A session a passkey made does without a check only through one: its
// person had a passkey, even if it is gone now.
func TestDashboardOperateOnTheWebAPasskeySessionNeedsACheck(t *testing.T) {
	r := newOperateRig(t, "5m")
	k := &softKey{rpID: webName, origin: r.web}
	s, reg := r.webLink()
	if code, body, _ := r.register(k, s, reg); code != http.StatusOK {
		t.Fatalf("registering a passkey: %d %s", code, body)
	}
	code, body, signed := r.signInWith(k, k.creds[0])
	if code != http.StatusOK {
		t.Fatalf("signing in with the passkey: %d %s", code, body)
	}
	if code, b := r.do(http.MethodDelete, "", dashboard.PasskeysPath+"/"+b64(k.creds[0].id), s, r.web, nil); code != http.StatusNoContent {
		t.Fatalf("removing the passkey: %d %s", code, b)
	}
	c := r.dial(t, r.web)
	c.WriteJSON(map[string]any{"session": signed.Session, "pane": r.pane})
	if m := readOp(t, c); m.Started != nil || !strings.Contains(m.Refused, "sign in again") {
		t.Errorf("a passkey's session without a check: %+v", m)
	}
}
