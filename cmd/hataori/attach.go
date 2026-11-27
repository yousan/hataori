package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"crypto/sha256"
	"encoding/hex"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/workerproto"
	"github.com/yousan/hataori/internal/workerssh"
	"net"
	"path/filepath"
)

// detachKey is the key that detaches a terminal from the agent: Ctrl-\. It
// is never passed to the agent.
const detachKey = 0x1c

// attachScreen bounds how much of the kept terminal log is replayed when a
// terminal attaches, so that the current screen is drawn quickly.
const attachScreen = 256 << 10

// attachStdin is what `hataori attach` reads keys from; tests change it.
var attachStdin io.Reader = os.Stdin

// attachTerminal is `hataori attach NUMBER`: it connects this terminal to the
// terminal of the thread's agent, through the thread's session host, until
// the person presses the detach key, their input ends, or the agent ends.
// The agent keeps running after a detach.
func attachTerminal(ctx context.Context, c control.API, t control.ThreadInfo, stdin io.Reader, stdout, stderr io.Writer) error {
	a, number, cleanup, err := openTerminalAttachment(ctx, c, t)
	if err != nil {
		return fmt.Errorf("thread %d: %w", t.ID, err)
	}
	defer cleanup()
	defer a.Close()
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		old, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(f.Fd()), old)
	}
	cols, rows := a.Size()
	fmt.Fprintf(stderr, "hataori: attached to thread %d (%dx%d); Ctrl-\\ detaches, the agent keeps running\r\n", number, cols, rows)

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
			return fmt.Errorf("thread %d: %w", number, err)
		}
		fmt.Fprintf(stderr, "\r\nhataori: detached from thread %d; the agent keeps running\r\n", number)
		return nil
	case err := <-ended:
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "\r\nhataori: the agent of thread %d ended\r\n", number)
			return nil
		}
		return fmt.Errorf("thread %d: %w", number, err)
	}
}

type terminalAttachment interface {
	Next() (host.TerminalChunk, error)
	Input([]byte) error
	Size() (int, int)
	Close() error
}

func openTerminalAttachment(ctx context.Context, c control.API, t control.ThreadInfo) (terminalAttachment, int64, func(), error) {
	noop := func() {}
	if t.Machine == "" || t.Machine == "local" {
		at, err := c.AttachTerminal(ctx, t.ID)
		if err != nil {
			return nil, 0, noop, err
		}
		a, err := host.Attach(at.Socket)
		return a, at.Number, noop, err
	}
	worker, ok := c.(control.WorkerAttacher)
	if !ok {
		return nil, 0, noop, fmt.Errorf("client cannot request a worker attachment")
	}
	at, err := worker.AttachWorker(ctx, t.ID)
	if err != nil {
		return nil, 0, noop, err
	}
	dir, err := os.MkdirTemp("/tmp", "hataori-owner-attach-")
	if err != nil {
		return nil, 0, noop, err
	}
	sum := sha256.Sum256([]byte(at.Destination))
	lock := filepath.Join(dir, hex.EncodeToString(sum[:])+".lock")
	var lockInfo os.FileInfo
	cleanup := func() {
		if lockInfo != nil {
			if now, err := os.Lstat(lock); err == nil && os.SameFile(now, lockInfo) {
				os.Remove(lock)
			}
		}
		os.Remove(dir)
	}
	pool, err := workerssh.New(ctx, workerssh.Options{RunDir: dir})
	if err != nil {
		cleanup()
		return nil, 0, noop, err
	}
	cleanupPool := func() { pool.Close(); cleanup() }
	binding := workerproto.Binding{Main: at.Main, Worker: at.Installation}
	w, err := pool.Worker(workerssh.Config{Destination: at.Destination, Root: at.Root, Binding: binding, AttachmentTarget: &workerproto.Target{Thread: at.Thread, Session: at.Session, Run: at.Run}})
	lockInfo, _ = os.Lstat(lock)
	if err != nil {
		cleanupPool()
		return nil, 0, noop, err
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, 15*time.Second)
	defer cancelReady()
	if err := w.WaitReady(readyCtx); err != nil {
		cleanupPool()
		return nil, 0, noop, err
	}
	remote := &machineio.Remote{Binding: binding, Target: workerproto.Target{Thread: at.Thread, Session: at.Session, Run: at.Run}, Open: func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		s, err := w.Open(ctx)
		if err != nil {
			return nil, nil, err
		}
		return s, s.Protocol, nil
	}}
	a, err := remote.Attach(ctx)
	if err != nil {
		cleanupPool()
		return nil, 0, noop, err
	}
	return a, at.Number, cleanupPool, nil
}
