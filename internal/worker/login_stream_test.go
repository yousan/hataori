package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/workerproto"
)

func loginResident(t *testing.T, mode string) (*fixture, *loginTestAdapter, func(context.Context) (net.Conn, *workerproto.Connection, error), *atomic.Int32) {
	return loginResidentDir(t, mode, false)
}

func loginResidentDir(t *testing.T, mode string, useDefault bool) (*fixture, *loginTestAdapter, func(context.Context) (net.Conn, *workerproto.Connection, error), *atomic.Int32) {
	t.Helper()
	f := newFixture(t, false)
	f.r.Close()
	a := &loginTestAdapter{mode: mode, health: agent.Health{Installed: true, LoggedIn: true}}
	f.options.NewAdapter = func(string, []byte) (agent.Adapter, error) { return a, nil }
	f.options.Config.Agents[0].ConfigDir = filepath.Join(f.root, "config/disposable")
	if useDefault {
		f.options.Config.Agents[0].ConfigDir = ""
	}
	f.options.LoginGuard = helperArgv("guard")
	f.options.LoginTimeout = 5 * time.Second
	f.open()
	path, err := f.r.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.r.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	count := &atomic.Int32{}
	open := func(ctx context.Context) (net.Conn, *workerproto.Connection, error) {
		count.Add(1)
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, nil, err
		}
		local := f.r.Hello()
		if err := sendFrame(c, local); err != nil {
			c.Close()
			return nil, nil, err
		}
		b, err := readFrame(bufio.NewReaderSize(c, workerproto.MaxFrame))
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		peer, err := workerproto.DecodeHello(b)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		p, err := workerproto.Negotiate(f.binding, local, peer)
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, p, nil
	}
	return f, a, open, count
}

func assertLoginMarkerAbsent(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, marker := range [][]byte{[]byte("marker-secret"), []byte("marker-device"), []byte("https://login.example.invalid/")} {
			encoded := []byte(base64.StdEncoding.EncodeToString(marker))
			if bytes.Contains(b, marker) || bytes.Contains(b, encoded) {
				t.Errorf("login marker persisted in %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkerLoginStreamUsesOnlySelectedResident(t *testing.T) {
	f, a, open, count := loginResident(t, "code")
	other, otherAdapter, _, _ := loginResident(t, "code")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := machineio.StartLogin(ctx, f.binding, open, f.configuration, f.options.Config.Agents[0].ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	result, link := false, false
	for {
		u, err := l.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		link = link || u.Link != ""
		if u.WantsCode {
			if err := l.Code("marker-secret"); err != nil {
				t.Fatal(err)
			}
		}
		result = result || u.Result == "logged_in"
	}
	if !link || !result || count.Load() != 1 {
		t.Fatal("login did not finish over exactly one live connection")
	}
	a.mu.Lock()
	if len(a.probes) != 1 || len(a.checks) != 1 || a.probes[0].ConfigDir != f.options.Config.Agents[0].ConfigDir || a.checks[0].ConfigDir != f.options.Config.Agents[0].ConfigDir {
		t.Error("login checked another configuration")
	}
	a.mu.Unlock()
	otherAdapter.mu.Lock()
	if len(otherAdapter.probes) != 0 || otherAdapter.launch.ConfigDir != "" {
		t.Error("another resident received the login")
	}
	otherAdapter.mu.Unlock()
	assertLoginMarkerAbsent(t, f.root)
	assertLoginMarkerAbsent(t, other.root)
}

func TestWorkerLoginDisconnectDoesNotReplay(t *testing.T) {
	f, _, open, count := loginResident(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := machineio.StartLogin(ctx, f.binding, open, f.configuration, f.options.Config.Agents[0].ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Next(); err != nil {
		t.Fatal(err)
	}
	l.Close()
	eventually(t, "disconnected login cleanup", func() bool {
		f.r.mu.Lock()
		defer f.r.mu.Unlock()
		return len(f.r.loginAttempts) == 0
	})
	if err := l.Code("marker-secret"); err == nil {
		t.Fatal("closed conversation accepted a code")
	}
	if count.Load() != 1 {
		t.Fatal("login reconnected implicitly")
	}
	// A fresh, explicit start opens another conversation; no previous input
	// or waiting state is supplied to the new process.
	next, err := machineio.StartLogin(ctx, f.binding, open, f.configuration, f.options.Config.Agents[0].ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	if count.Load() != 2 {
		t.Fatal("fresh press did not open exactly one new conversation")
	}
	assertLoginMarkerAbsent(t, f.root)
}
