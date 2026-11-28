package desktopstate

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/host"
)

func TestHostCompatibilityRefusesAmbiguousOrUnprovenOwnershipWithoutPIDs(t *testing.T) {
	for _, kind := range []string{"ambiguous", "link", "unknown-owner", "held-without-capability", "idle"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, rootFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			base := filepath.Join(s.state.Name(), "sessions")
			os.Mkdir(base, 0o700)
			name := "1"
			if kind == "ambiguous" {
				name = "01"
			}
			dir := filepath.Join(base, name)
			os.Mkdir(dir, 0o700)
			var file *os.File
			switch kind {
			case "link":
				os.Remove(dir)
				os.Symlink(rootFixture(t), dir)
			case "unknown-owner":
				os.WriteFile(filepath.Join(dir, "host.json"), []byte("inert existing spec"), 0o600)
			case "held-without-capability", "idle":
				file, err = os.OpenFile(filepath.Join(dir, "host.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if kind == "held-without-capability" {
					if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
					defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				}
			}
			err = s.CheckHosts(ctx)
			if kind == "idle" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unproven host accepted")
			}
		})
	}
}

// Only synthetic, scratch Unix peers are used. No host/agent process is
// launched, discovered, signaled or borrowed from an existing instance.
func TestHostCompatibilityBindsComputedPrivateEndpoint(t *testing.T) {
	for _, kind := range []string{"direct", "long-path", "legacy-alias-pointer", "redirected-pointer", "socket-link", "socket-mode", "runtime-mode"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root := rootFixture(t)
			if kind == "long-path" || kind == "legacy-alias-pointer" {
				root = filepath.Join(root, strings.Repeat("long", 30))
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			dir := filepath.Join(s.state.Name(), "sessions/1")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, host.SpecFile), []byte(`{"argv":["inert"]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			lease, err := os.OpenFile(filepath.Join(dir, host.LockFile), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			defer syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
			expected := host.ComputedSocketPath(dir)
			endpoint := expected
			if kind == "legacy-alias-pointer" {
				// The old producer hashes its unnormalized spelling. A selected
				// canonical root cannot adopt that different endpoint by pointer.
				alias := filepath.Join(rootFixture(t), strings.Repeat("alias", 15))
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				endpoint = host.ComputedSocketPath(filepath.Join(alias, "state/sessions/1"))
				if endpoint == expected {
					t.Fatal("fixture did not exercise the legacy alias")
				}
			}
			if kind == "long-path" || kind == "legacy-alias-pointer" {
				if err := os.Mkdir(filepath.Dir(endpoint), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(filepath.Dir(endpoint)) })
			}
			if kind == "redirected-pointer" || kind == "socket-link" {
				endpoint = filepath.Join(rootFixture(t), "unrelated.sock")
			}
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			if err := os.Chmod(endpoint, 0o600); err != nil {
				t.Fatal(err)
			}
			served := make(chan struct{})
			go func() {
				defer close(served)
				conn, err := ln.AcceptUnix()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				var request struct {
					Op string `json:"op"`
				}
				if json.NewDecoder(conn).Decode(&request) == nil && request.Op == "input-capability" {
					json.NewEncoder(conn).Encode(map[string]any{"ok": true, "input_capability": host.InputCapability{Version: host.InputBatchVersion, Run: "synthetic-owned-host"}})
				}
			}()
			defer func() { ln.Close(); <-served }()
			pointer := filepath.Join(dir, "host.sock.path")
			if kind == "redirected-pointer" || kind == "long-path" || kind == "legacy-alias-pointer" {
				if err := os.WriteFile(pointer, []byte(endpoint+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "socket-link" {
				if err := os.Symlink(endpoint, expected); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "socket-mode" {
				if err := os.Chmod(endpoint, 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "runtime-mode" {
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err = s.CheckHosts(ctx)
			if kind == "direct" || kind == "long-path" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unbound/untrusted host endpoint accepted")
			}
			if kind == "redirected-pointer" || kind == "legacy-alias-pointer" {
				data, err := os.ReadFile(pointer)
				if err != nil || string(data) != endpoint+"\n" {
					t.Fatal("refusal changed pointer metadata")
				}
			}
		})
	}
}
