package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// A thread whose scope is no longer in the config stays reachable by its
// number (design, section 6.2): the owner on the control socket and in the
// dashboard still lists and reads it, and can stop, close and remove it.
// Nothing starts in that scope.
func TestAThreadOfAScopeNoLongerInTheConfigStaysReachable(t *testing.T) {
	scope := func(key string) map[string]any {
		return map[string]any{"key": key, "client": fakeSurface, "enabled": true, "posters": []string{e2eHook}}
	}
	r := newChatRoot(t, func(cfg map[string]any) {
		cfg["scopes"] = []map[string]any{scope("c1"), scope("c2")}
		cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
	})
	r.deadline = time.Now().Add(90 * time.Second)

	// The first run, with both scopes: in c2 one thread idle after a turn,
	// and one with a turn that runs on.
	stop := runDaemon(t, r.root)
	r.connected(t)
	c2 := surface.Conversation{Channel: "c2"}
	opened := func(n int) (th surface.Conversation) {
		t.Helper()
		eventually(t, "a thread in c2", r.within(15*time.Second), func() bool {
			var found []surface.Conversation
			for _, c := range r.driver.Calls(c2) {
				if c.Op == fakesurface.OpOpenThread {
					found = append(found, c.Opened)
				}
			}
			if len(found) > n {
				th = found[n]
			}
			return len(found) > n
		})
		return th
	}
	r.driver.Inject(c2, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply first")
	th := opened(0)
	r.posted(t, th, "first")
	number := ""
	eventually(t, "the thread to be idle", r.within(15*time.Second), func() bool {
		number = numberOf(t, r.root, th)
		return number != "" && r.state(t, th) == "idle"
	})
	r.driver.Inject(c2, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" tool wait 60s\nreply never")
	long := opened(1)
	longNumber := ""
	eventually(t, "the other thread to run", r.within(15*time.Second), func() bool {
		longNumber = numberOf(t, r.root, long)
		return longNumber != "" && r.state(t, long) == "running"
	})
	stop()

	// The operator takes c2 out of the config and starts the daemon again.
	setConfig(t, r.root, func(cfg map[string]any) { cfg["scopes"] = []map[string]any{scope("c1")} })
	startDaemon(t, r.root)

	_, out, _ := cli(t, "threads", "--root", r.root)
	for _, n := range []string{number, longNumber} {
		if !strings.Contains(out, `"thread":`+n+",") {
			t.Errorf("hataori threads does not list thread %s of the scope that left the config: %q", n, out)
		}
	}
	code, out, errOut := cli(t, "thread", "--root", r.root, number)
	if code != 0 {
		t.Errorf("hataori thread %s: exit %d, %q %q", number, code, out, errOut)
	}
	base, cookie := dashSignIn(t, r.root)
	if code, body := dashGet(t, base, cookie, "/api/threads"); code != http.StatusOK || !strings.Contains(body, `"c2"`) {
		t.Errorf("the owner's dashboard: %d %s, want the threads of c2", code, body)
	}

	// Nothing starts in c2.
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, _ := strconv.ParseInt(number, 10, 64)
	if _, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "c2", Thread: "new", Repo: bareRepo(t), Input: control.Input{Text: "reply no"}}); !errors.Is(err, core.ErrDisabled) {
		t.Errorf("start in the scope that left the config: %v, want disabled", err)
	}
	if err := c.Send(ctx, control.ThreadID(id), "k2", control.Input{Text: "reply no"}, control.Deliver); !errors.Is(err, core.ErrDisabled) {
		t.Errorf("send in the scope that left the config: %v, want disabled", err)
	}

	code, out, errOut = cli(t, "stop", "--root", r.root, longNumber)
	if code != 0 {
		t.Errorf("hataori stop %s: exit %d, %q %q", longNumber, code, out, errOut)
	}
	eventually(t, "the stopped thread to be idle", r.within(20*time.Second), func() bool {
		_, out, _ := cli(t, "thread", "--root", r.root, longNumber)
		return strings.Contains(out, `"state":"idle"`)
	})
	code, out, errOut = cli(t, "close", "--root", r.root, number)
	if code != 0 {
		t.Errorf("hataori close %s: exit %d, %q %q", number, code, out, errOut)
	}
	if code, out, errOut := cli(t, "reopen", "--root", r.root, number); code == 0 || !strings.Contains(errOut, "not enabled") {
		t.Errorf("hataori reopen %s: exit %d, %q %q, want refused", number, code, out, errOut)
	}
	ws := filepath.Join(r.root, "workspaces")
	before, _ := filepath.Glob(filepath.Join(ws, "*"))
	code, out, errOut = cli(t, "remove", "--root", r.root, number)
	if code != 0 || !strings.Contains(out, `"state":"removed"`) {
		t.Errorf("hataori remove %s: exit %d, %q %q", number, code, out, errOut)
	}
	if after, _ := os.ReadDir(ws); len(after) != len(before)-1 {
		t.Errorf("workspaces after the removal: %d, before %d", len(after), len(before))
	}
}

// The owner's dashboard caller has the scopes that left the config; another
// person's has none of them.
func TestDashboardCallerOfScopesThatLeft(t *testing.T) {
	dir := people.New([]people.Person{{Name: "ada"}, {Name: "grace"}}, "ada", map[string][]string{"x": {"grace"}})
	for person, want := range map[string][]string{"ada": {"s", "x", "gone"}, "grace": {"x"}} {
		if c := dashboardCaller(dir, []string{"s", "x", "gone"}, false, false, person); !slices.Equal(c.Scopes, want) {
			t.Errorf("%s: %v, want %v", person, c.Scopes, want)
		}
	}
}
