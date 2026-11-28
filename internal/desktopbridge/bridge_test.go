package desktopbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestRelayRetainsBytesAndJoinsOnPeerEnd(t *testing.T) {
	client, server := net.Pipe()
	input, keys := io.Pipe()
	var output bytes.Buffer
	payload := bytes.Repeat([]byte{0, 1, 255, '\n', '\r'}, 40000)
	done := make(chan error, 1)
	go func() { done <- relay(context.Background(), client, input, &output) }()
	written := make(chan error, 1)
	go func() { _, err := keys.Write(payload); written <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(server, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("request bytes changed: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write(payload); err != nil {
		t.Fatal(err)
	}
	server.Close()
	select {
	case err := <-done:
		if err != nil || !bytes.Equal(output.Bytes(), payload) {
			t.Fatalf("response or join: %d bytes, %v", output.Len(), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay retained a blocked input producer")
	}
	if _, err := keys.Write([]byte("must never replay")); err == nil {
		t.Fatal("input remained usable after peer end")
	}
}

func TestRelayCancellationJoinsBlockedInput(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	input, keys := io.Pipe()
	defer keys.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay(ctx, client, input, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not join producer")
	}
}
