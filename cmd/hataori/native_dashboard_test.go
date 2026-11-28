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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/version"
)

// This fixture runs no agents, chat services or tmux. All state is disposable.
func runNativeFixture(t *testing.T, root string) (*http.Client, nativehttp.Config, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var output bytes.Buffer
	var mu sync.Mutex
	var code int
	go func() {
		defer close(done)
		code = runCtx(ctx, []string{"run", "--native", "--root", root}, io.Discard, lockedWriter{&mu, &output})
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	var cfg nativehttp.Config
	deadline := time.Now().Add(5 * time.Second)
	for {
		id, err := store.ReadMainIdentity(context.Background(), root)
		cfg = nativehttp.Config{Root: root, Instance: id, Version: version.String()}
		if err == nil {
			c, err := nativehttp.Dial(ctx, cfg)
			if err == nil {
				// A bound socket is not readiness: bind intentionally precedes
				// HTTP acceptance and control-socket construction.
				c.SetDeadline(time.Now().Add(time.Second))
				_, err = io.WriteString(c, "GET /api/native HTTP/1.1\r\nHost: instance\r\nConnection: close\r\n\r\n")
				var response *http.Response
				if err == nil {
					response, err = http.ReadResponse(bufio.NewReader(c), nil)
				}
				ready := err == nil && response.StatusCode == http.StatusOK
				if response != nil {
					response.Body.Close()
				}
				c.Close()
				if ready {
					break
				}
			}
		}
		select {
		case <-done:
			mu.Lock()
			logged := output.String()
			mu.Unlock()
			if runtime.GOOS == "darwin" && strings.Contains(logged, nativehttp.ErrACLUnavailable.Error()) {
				t.Skip("native access fails closed in this Darwin build without ACL inspection")
			}
			t.Fatalf("native daemon ended with %d: %s", code, logged)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("native daemon did not serve")
		}
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return nativehttp.Dial(ctx, cfg) }}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 3 * time.Second}, cfg, stop
}

func nativeFixtureRoot(t *testing.T) string {
	t.Helper()
	root := newRoot(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func getNativeFixture(t *testing.T, c *http.Client, path string) (int, []byte) {
	t.Helper()
	r, err := c.Get("http://instance" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, b
}

func TestNativeDaemonLifecycle(t *testing.T) {
	root := nativeFixtureRoot(t)
	stopBrowserless := runDaemon(t, root)
	if _, err := os.Stat(filepath.Join(root, "state", nativehttp.DiscoveryFile)); !os.IsNotExist(err) {
		t.Fatalf("native listener enabled without --native: %v", err)
	}
	stopBrowserless()
	callers := make(chan control.Caller, 4)
	dashboardHook = func(c *control.Caller) { callers <- *c }
	t.Cleanup(func() { dashboardHook = nil })
	c, cfg, stop := runNativeFixture(t, root)
	caller := <-callers
	if code, b := getNativeFixture(t, c, dashboard.MePath); code != 200 || !bytes.Contains(b, []byte(`"person":"owner"`)) || !bytes.Contains(b, []byte(`"way":"native"`)) {
		t.Fatalf("native owner: %d %s", code, b)
	}
	if caller.Principal != "owner" || caller.Via != "dashboard" || !caller.May.Has(control.OpSend) || caller.May&(control.OpSignIn|control.OpRemove|control.OpReopen) != 0 || len(caller.Scopes) != 1 || caller.Scopes[0] != "s" {
		t.Fatalf("native caller: %+v", caller)
	}
	if code, b := getNativeFixture(t, c, dashboard.ThreadsPath); code != 200 || string(bytes.TrimSpace(b)) != "[]" {
		t.Fatalf("native reads: %d %s", code, b)
	}
	discovery := filepath.Join(root, "state", nativehttp.DiscoveryFile)
	before, err := os.ReadFile(discovery)
	if err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.Stat(filepath.Join(root, instance.LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, "run", "--native", "--root", root); code != exitError || !strings.Contains(errOut, "holds the lock") {
		t.Fatalf("duplicate daemon: %d %s", code, errOut)
	}
	incompatible := cfg
	incompatible.Version = "incompatible"
	if conn, err := nativehttp.Dial(context.Background(), incompatible); err == nil {
		conn.Close()
		t.Fatal("incompatible instance accepted")
	}
	after, _ := os.ReadFile(discovery)
	lockAfter, _ := os.Stat(filepath.Join(root, instance.LockFile))
	if !bytes.Equal(before, after) || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("busy/incompatible access replaced discovery or instance lock")
	}
	stop()
	if conn, err := nativehttp.Dial(context.Background(), cfg); err == nil {
		conn.Close()
		t.Fatal("native listener outlived daemon")
	}
	in, err := instance.Open(root)
	if err != nil {
		t.Fatalf("shutdown retained root lock: %v", err)
	}
	in.Close()
	c2, cfg2, stop2 := runNativeFixture(t, root)
	defer stop2()
	if cfg2.Instance != cfg.Instance {
		t.Fatal("restart changed installation identity")
	}
	if code, _ := getNativeFixture(t, c2, dashboard.MePath); code != 200 {
		t.Fatalf("restart native: %d", code)
	}
	restarted, _ := os.ReadFile(discovery)
	if bytes.Equal(before, restarted) {
		t.Fatal("restart reused socket generation")
	}
}

func TestNativeDaemonKeepsBrowserSignIn(t *testing.T) {
	root := nativeFixtureRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	c, _, stop := runNativeFixture(t, root)
	defer stop()
	base, session := dashSignIn(t, root)
	if code, _ := dashGet(t, base, "", dashboard.MePath); code != 401 {
		t.Fatalf("unsigned browser: %d", code)
	}
	if code, b := dashGet(t, base, session, dashboard.MePath); code != 200 || !strings.Contains(b, `"way":"local"`) {
		t.Fatalf("signed browser: %d %s", code, b)
	}
	if code, b := getNativeFixture(t, c, dashboard.NativeIdentityPath); code != 200 {
		t.Fatalf("native identity: %d %s", code, b)
	} else {
		var identity struct {
			Instance, Version string
			Native            bool
		}
		if json.Unmarshal(b, &identity) != nil || identity.Instance == "" || identity.Version != version.String() || !identity.Native {
			t.Fatalf("identity: %s", b)
		}
	}
}

func TestNativeFlagAndUnsafeRootRefuse(t *testing.T) {
	root := nativeFixtureRoot(t)
	if code, _, _ := cli(t, "threads", "--native", "--root", root); code != exitUsage {
		t.Fatalf("--native accepted on another command: %d", code)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, "run", "--native", "--root", root); code != exitError || !strings.Contains(errOut, "native dashboard") {
		t.Fatalf("unsafe root: %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "state", nativehttp.DiscoveryFile)); !os.IsNotExist(err) {
		t.Fatalf("unsafe root published discovery: %v", err)
	}
	in, err := instance.Open(root)
	if err != nil {
		t.Fatalf("failed startup retained lock: %v", err)
	}
	in.Close()
}
