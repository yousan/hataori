package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
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
