// Package desktopbridge carries one checked native HTTP connection over the
// bundled helper's private stdio. It has no TCP endpoint or arbitrary path,
// command, identity-header or socket-forwarding operation.
package desktopbridge

import (
	"context"
	"errors"
	"io"
	"net"
	"os"

	"github.com/yousan/hataori/internal/nativehttp"
)

// Relay supports HTTP response streaming and WebSocket upgrades without a
// second protocol or buffer. Pipe/socket backpressure is retained. Each child
// has one connection; a lost child cannot retry or replay an uncertain write.
func Relay(ctx context.Context, cfg nativehttp.Config, input io.ReadCloser, output io.Writer) error {
	conn, err := nativehttp.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	return relay(ctx, conn, input, output)
}

func relay(ctx context.Context, conn net.Conn, input io.ReadCloser, output io.Writer) error {
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
		input.Close()
	})
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, input)
		if half, ok := conn.(interface{ CloseWrite() error }); ok {
			half.CloseWrite()
		} else {
			conn.Close()
		}
		done <- err
	}()
	_, err := io.Copy(output, conn)
	conn.Close()
	input.Close()
	inputErr := <-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if errors.Is(inputErr, os.ErrClosed) || errors.Is(inputErr, io.ErrClosedPipe) {
		return nil
	}
	return inputErr
}
