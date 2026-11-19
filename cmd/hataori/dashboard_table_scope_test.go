package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/wire"
)

// The All threads table's group headers say what the tree's nodes say: the
// channel's name when the surface reports one, the key when it does not,
// and the config's name over the channel's; for a caller that may not read
// text, the config's name or the key. Driven by the real server with the
// fake surface and the fake agent.
func TestDashboardTableScopeNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		noText bool
		want   map[string]string
	}{
		{"may read text", false, map[string]string{"c1": "#builds", "c2": "c2", "c3": "Release"}},
		{"may not read text", true, map[string]string{"c1": "c1", "c2": "c2", "c3": "Release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newChatRoot(t, func(cfg map[string]any) {
				cfg["scopes"] = []map[string]any{
					{"key": "c1", "client": fakeSurface, "enabled": true},
					{"key": "c2", "client": fakeSurface, "enabled": true},
					{"key": "c3", "client": fakeSurface, "enabled": true, "name": "Release"},
				}
				cfg["dashboard"] = map[string]any{"listen": "127.0.0.1:0"}
			})
			r.driver.SetName(surface.Conversation{Channel: "c1"}, "#builds")
			r.driver.SetName(surface.Conversation{Channel: "c3"}, "#rel")
			if tc.noText {
				dashboardHook = func(c *control.Caller) { c.NoText = true }
				t.Cleanup(func() { dashboardHook = nil })
			}
			runDaemon(t, r.root)
			r.connected(t)
			c, err := wire.Dial(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := context.Background()
			repo := bareRepo(t)
			for _, scope := range []string{"c1", "c2", "c3"} {
				id, err := c.Start(ctx, control.StartRequest{Key: "k-" + scope, Scope: scope, Repo: repo, Input: control.Input{Text: "reply " + scope}})
				if err != nil {
					t.Fatal(err)
				}
				eventually(t, "the thread", 20*time.Second, func() bool { d, err := c.Thread(ctx, id, 0, 0); return err == nil && d.State == control.Idle })
			}
			// The surface's names reach the core first.
			eventually(t, "the channel's name recorded", 10*time.Second, func() bool {
				ss, _ := c.Scopes(ctx)
				return len(ss) == 3 && ss[0].Name == "#builds"
			})
			b := newBrowser(t)
			link, err := c.SignInLink(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			b.call("Page.navigate", map[string]any{"url": link})
			for scope, want := range tc.want {
				b.until(fmt.Sprintf("the header of %s as %q, as the tree's node", scope, want), fmt.Sprintf(`(() => {
					const h = document.querySelector('.all-threads tbody[data-scope=%[1]q] .scope-row th');
					const n = document.querySelector('.tree [data-node="scope:%[1]s"] .node-head .scope-name');
					return h?.textContent === %[2]q && n?.textContent === %[2]q;
				})()`, scope, want))
			}
		})
	}
}
