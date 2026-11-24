package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The daemon's chat front end whose driver cannot connect at first, as
// when the daemon starts before the network is up (#598): while it tries
// again, `hataori threads` says so on standard error, /api/clients and the
// status bar say so; a trigger posted meanwhile starts its thread once it
// connects; and then nothing says it is down.
func TestFrontEndThatCannotConnectYet(t *testing.T) {
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	// 1 s, 2 s and 4 s apart: it connects 7 s after the daemon starts.
	r.driver.Fail(fakesurface.OpRun, 3, errors.New("network is unreachable"))
	b := newBrowser(t) // before the daemon: the page has the 7 s to load
	_, logged := runDaemonLog(t, r.root)
	eventually(t, "the first failure", 10*time.Second, func() bool { return r.driver.Runs() >= 1 })
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply hello there")
	down := "hataori: chat client " + fakeSurface + " is not connected since "

	if _, _, errOut := cli(t, "threads", "--root", r.root); !strings.Contains(errOut, down) {
		t.Fatalf("while it cannot connect, threads says %q on standard error", errOut)
	}
	base, session := dashSignIn(t, r.root)
	code, body := dashGet(t, base, session, "/api/clients")
	var cs []control.ClientState
	if err := json.Unmarshal([]byte(body), &cs); code != http.StatusOK || err != nil || len(cs) != 1 ||
		cs[0].Client != fakeSurface || cs[0].Connected {
		t.Fatalf("/api/clients: %d %s", code, body)
	}
	_, out, _ := cli(t, "signin", "--root", r.root)
	var link struct{ Link string }
	json.Unmarshal([]byte(out), &link)
	b.call("Page.navigate", map[string]any{"url": link.Link})
	b.until("the client in the status bar", `!!document.querySelector('.status .client-down[data-client="`+fakeSurface+`"]')`)
	if r.driver.Connected() {
		t.Fatal("the driver connected before the page showed it was not")
	}

	r.posted(t, r.thread(t, 0), "hello there")
	if _, _, errOut := cli(t, "threads", "--root", r.root); strings.Contains(errOut, down) {
		t.Fatalf("connected, threads says %q on standard error", errOut)
	}
	b.until("the status bar without it", `!document.querySelector('.status .client-down')`)
	log := logged()
	if n, m := strings.Count(log, "the driver stopped; trying again"), strings.Count(log, "chat front end connected"); n != 1 || m != 1 {
		t.Fatalf("logged %d failure lines and %d connection lines, want 1 and 1:\n%s", n, m, log)
	}
}

// A driver whose credential is another identity's ends the daemon with one
// line that names both; nothing is opened on the chat service, and the
// daemon runs again once the credential is right (#566).
func TestDaemonEndsWithAnotherIdentity(t *testing.T) {
	r := newChatRoot(t, nil)
	reason := "the token is for bot 1, but the config names bot 2"
	r.driver.Fail(fakesurface.OpIdentity, 1, fmt.Errorf("%s: %w", reason, surface.ErrOtherIdentity))
	var log bytes.Buffer
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := runCtx(ctx, []string{"run", "--root", r.root}, io.Discard, lockedWriter{&mu, &log})
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil || code != exitError {
		t.Fatalf("run returned %d (%v), want %d by itself; it logged:\n%s", code, ctx.Err(), exitError, log.String())
	}
	last := log.String()[strings.LastIndex(strings.TrimSpace(log.String()), "\n")+1:]
	if strings.Count(log.String(), reason) != 1 || !strings.HasPrefix(last, "hataori: ") || !strings.Contains(last, reason) {
		t.Fatalf("it logged:\n%s", log.String())
	}
	if c := r.driver.Calls(c1); len(c) != 0 || r.driver.Runs() != 0 {
		t.Fatalf("%d runs of the driver, calls %+v", r.driver.Runs(), c)
	}

	runDaemon(t, r.root)
	r.connected(t)
}
