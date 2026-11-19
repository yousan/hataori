package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
)

// detachKey is the key that detaches a terminal from the agent: Ctrl-\. It
// is never passed to the agent.
const detachKey = 0x1c

// attachScreen bounds how much of the kept terminal log is replayed when a
// terminal attaches, so that the current screen is drawn quickly.
const attachScreen = 256 << 10

// attachStdin is what `hataori attach` reads keys from; tests change it.
var attachStdin io.Reader = os.Stdin

// attachTerminal is `hataori attach KEY`: it connects this terminal to the
// terminal of the thread's agent, through the thread's session host, until
// the person presses the detach key, their input ends, or the agent ends.
// The agent keeps running after a detach.
func attachTerminal(ctx context.Context, c control.API, t control.ThreadInfo, stdin io.Reader, stdout, stderr io.Writer) error {
	at, err := c.AttachTerminal(ctx, t.ID)
	if err != nil {
		return err
	}
	a, err := host.Attach(at.Socket)
	if err != nil {
		return fmt.Errorf("thread %d: %w", at.Number, err)
	}
	defer a.Close()
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		old, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(f.Fd()), old)
	}
	cols, rows := a.Size()
	fmt.Fprintf(stderr, "hataori: attached to thread %d (%dx%d); Ctrl-\\ detaches, the agent keeps running\r\n", at.Number, cols, rows)

	ended := make(chan error, 1)
	go func() {
		for first := true; ; first = false {
			ch, err := a.Next()
			if err != nil {
				ended <- err
				return
			}
			data := ch.Data
			if first || ch.Reset {
				if len(data) > attachScreen {
					data = data[len(data)-attachScreen:]
				}
				data = append([]byte("\x1b[H\x1b[2J"), data...)
			}
			if _, err := stdout.Write(data); err != nil {
				ended <- err
				return
			}
		}
	}()
	detached := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			b := buf[:n]
			i := bytes.IndexByte(b, detachKey)
			if i >= 0 {
				b = b[:i]
			}
			if len(b) > 0 {
				if werr := a.Input(b); werr != nil {
					detached <- werr
					return
				}
			}
			if i >= 0 || err != nil {
				detached <- nil // the detach key, or the end of the input
				return
			}
		}
	}()
	select {
	case err := <-detached:
		a.Close()
		if err != nil {
			return fmt.Errorf("thread %d: %w", at.Number, err)
		}
		fmt.Fprintf(stderr, "\r\nhataori: detached from thread %d; the agent keeps running\r\n", at.Number)
		return nil
	case err := <-ended:
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "\r\nhataori: the agent of thread %d ended\r\n", at.Number)
			return nil
		}
		return fmt.Errorf("thread %d: %w", at.Number, err)
	}
}
