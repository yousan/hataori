package machineio

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/workerproto"
)

func TestSessionEffectRefusesOldResidentBeforeSending(t *testing.T) {
	binding := workerproto.Binding{Main: "main", Worker: "worker"}
	h := workerproto.Hello{Version: "1.1", Binding: binding, Capabilities: []string{"runner", "host", "receipts"}}
	p, err := workerproto.Negotiate(binding, h, h)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer server.Close()
	read := make(chan int, 1)
	go func() { var b [4096]byte; n, _ := server.Read(b[:]); read <- n; server.Close() }()
	r := &Remote{Binding: binding, Target: workerproto.Target{Thread: 1, Session: "s-1", Run: "run"}, Open: func(context.Context) (net.Conn, *workerproto.Connection, error) { return client, p, nil }}
	m, err := r.Request("start", "start", workerproto.Start{AgentConfig: workerproto.AgentConfig{Agent: "fake", Config: "default"}, Holder: "own", Workspace: "workspace", NoProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	var nothing *holder.NotSent
	if _, err := r.Effect(context.Background(), m); !errors.As(err, &nothing) {
		t.Fatalf("missing explicit pre-send refusal: %v", err)
	}
	if n := <-read; n != 0 {
		t.Fatalf("sent %d bytes to an incompatible resident", n)
	}
}
