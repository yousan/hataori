package main

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestDesktopChildLifetimePipeCancelsOnlyDaemonAndJoins(t *testing.T) {
	input, launcher := io.Pipe()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- desktopChild(context.Background(), "isolated-root", input, func(ctx context.Context, root string) int {
			if root != "isolated-root" {
				t.Error("root changed")
			}
			close(started)
			<-ctx.Done()
			return 0
		})
	}()
	<-started
	launcher.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed launcher pipe retained daemon or reader")
	}
}

func TestDesktopChildEarlyReturnJoinsParentReader(t *testing.T) {
	input, launcher := io.Pipe()
	defer launcher.Close()
	done := make(chan error, 1)
	go func() {
		done <- desktopChild(context.Background(), "isolated-root", input, func(context.Context, string) int { return 1 })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("child failure hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("early return left parent reader blocked")
	}
}
