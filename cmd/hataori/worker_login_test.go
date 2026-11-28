package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/workerproto"
)

func TestWorkerLoginDaemonUsesPinnedTransport(t *testing.T) {
	root := newChatRoot(t, nil)
	binding := workerproto.Binding{Main: "fixture-main", Worker: "fixture-pinned-worker"}
	var opens atomic.Int32
	refused := errors.New("isolated transport observation")
	coreHook = func(c *core.Core) {
		// Replace only this isolated runner's transport; no SSH, user
		// credentials, agent commands or existing process is used.
		c.Runner.(*runner.Runner).Remotes = map[string]runner.RemoteMachine{"build": {
			Binding: binding,
			Open: func(context.Context) (net.Conn, *workerproto.Connection, error) {
				opens.Add(1)
				return nil, nil, refused
			},
		}}
	}
	relays := make(chan *relay.Relay, 1)
	frontEndHook = func(fe *chat.FrontEnd) { relays <- fe.Relay.(*relay.Relay) }
	t.Cleanup(func() { coreHook, frontEndHook = nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon(ctx, root.root, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case lr := <-relays:
		eventually(t, "the fixture surface", 5*time.Second, root.driver.Connected)
		endpoint, ok := lr.Workers["build"]
		if !ok || endpoint.Binding != binding || endpoint.Open == nil || len(lr.Workers) != 1 {
			t.Fatal("daemon did not give the login relay its pinned worker transport")
		}
		if _, _, err := endpoint.Open(ctx); !errors.Is(err, refused) || opens.Load() != 1 {
			t.Fatal("login relay did not retain the configured transport")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not construct the fixture login relay")
	}
}
