package nativehttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Config {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "native-http-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if errors.Is(checkACL(root), ErrACLUnavailable) {
		t.Skip("this build cannot verify Darwin ACLs; native transport is disabled")
	}
	return Config{Root: root, Instance: "installation-A", Version: "test-1"}
}

func listenFixture(t *testing.T, cfg Config) *Listener {
	t.Helper()
	l, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func discoveryPath(cfg Config) string { return filepath.Join(cfg.Root, "state", DiscoveryFile) }

func changeDiscovery(t *testing.T, cfg Config, change func(*discovery)) {
	t.Helper()
	b, err := os.ReadFile(discoveryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var d discovery
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	change(&d)
	b, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, discoveryPath(cfg), b)
}

func TestPrivateHTTPRoundTripAndLease(t *testing.T) {
	cfg := fixture(t)
	l := listenFixture(t, cfg)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("private response"))
	})}
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return Dial(ctx, cfg) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("http://instance/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "private response" {
		t.Fatalf("response %q, %v", body, err)
	}
	if other, err := Listen(cfg); !errors.Is(err, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("duplicate listener = %v; want busy", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
	// An old Close must not remove a replacement's socket or discovery.
	newListener := listenFixture(t, cfg)
	l.Close()
	conn, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	newListener.Close()
}

func TestSocketInitializationFailureCleansItsOwnSocket(t *testing.T) {
	cfg := fixture(t)
	var created string
	want := errors.New("fixture: cannot set socket mode")
	if l, err := listen(cfg, func(path string) error { created = path; return want }); !errors.Is(err, want) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("initialization failure: %v", err)
	}
	if created == "" {
		t.Fatal("fixture did not reach socket creation")
	}
	if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed initialization left socket: %v", err)
	}
	if _, err := os.Lstat(discoveryPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published failed listener: %v", err)
	}
	listenFixture(t, cfg) // the construction failure also released its lease
}

func TestRefusePrivateDirectoryFailures(t *testing.T) {
	for _, which := range []string{"root-mode", "root-link", "state-mode", "state-link", "lease-mode", "lease-link", "lease-relative-link", "lease-hardlink", "discovery-link", "discovery-relative-link", "discovery-hardlink"} {
		t.Run(which, func(t *testing.T) {
			cfg := fixture(t)
			if err := os.Mkdir(filepath.Join(cfg.Root, "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(cfg.Root, "state")
			target := filepath.Join(cfg.Root, "untouched")
			mustWrite(t, target, []byte("keep"))
			switch which {
			case "root-mode":
				if err := os.Chmod(cfg.Root, 0o750); err != nil {
					t.Fatal(err)
				}
			case "root-link":
				link := cfg.Root + "-link"
				if err := os.Symlink(cfg.Root, link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(link) })
				cfg.Root = link
			case "state-mode":
				if err := os.Chmod(state, 0o750); err != nil {
					t.Fatal(err)
				}
			case "state-link":
				if err := os.Rename(state, state+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("state-real", state); err != nil {
					t.Fatal(err)
				}
			case "lease-mode", "lease-link", "lease-relative-link", "lease-hardlink", "discovery-link", "discovery-relative-link", "discovery-hardlink":
				name := filepath.Join(state, leaseFile)
				if strings.HasPrefix(which, "discovery") {
					name = filepath.Join(state, DiscoveryFile)
				}
				if strings.HasSuffix(which, "-link") {
					linkTarget := target
					if strings.Contains(which, "relative") {
						linkTarget = "private-target"
						mustWrite(t, filepath.Join(state, linkTarget), []byte("keep"))
					}
					if err := os.Symlink(linkTarget, name); err != nil {
						t.Fatal(err)
					}
				} else if strings.HasSuffix(which, "-hardlink") {
					if err := os.Link(target, name); err != nil {
						t.Fatal(err)
					}
				} else {
					mustWrite(t, name, nil)
					if err := os.Chmod(name, 0o640); err != nil {
						t.Fatal(err)
					}
				}
			}
			if l, err := Listen(cfg); err == nil {
				l.Close()
				t.Fatal("accepted unsafe private state")
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "keep" {
				t.Fatalf("changed unrelated file: %q, %v", b, err)
			}
		})
	}
}

func TestDiscoveryRefusesChangedBindings(t *testing.T) {
	for _, which := range []string{"root", "installation", "version", "socket", "inode", "generation", "format", "mode", "link", "relative-link", "hardlink", "oversize", "trailing", "missing"} {
		t.Run(which, func(t *testing.T) {
			cfg := fixture(t)
			l := listenFixture(t, cfg)
			switch which {
			case "mode":
				if err := os.Chmod(discoveryPath(cfg), 0o640); err != nil {
					t.Fatal(err)
				}
			case "link", "relative-link", "hardlink":
				path := discoveryPath(cfg)
				if err := os.Rename(path, path+"-real"); err != nil {
					t.Fatal(err)
				}
				var err error
				if which == "relative-link" {
					err = os.Symlink(filepath.Base(path)+"-real", path)
				} else if which == "link" {
					err = os.Symlink(path+"-real", path)
				} else {
					err = os.Link(path+"-real", path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(discoveryPath(cfg)); err != nil {
					t.Fatal(err)
				}
			case "oversize", "trailing":
				f, err := os.OpenFile(discoveryPath(cfg), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if which == "oversize" {
					_, err = f.WriteString(strings.Repeat(" ", 8192))
				} else {
					_, err = f.WriteString("{}")
				}
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			default:
				changeDiscovery(t, cfg, func(d *discovery) {
					switch which {
					case "root":
						d.Root += "-other"
					case "installation":
						d.Instance = "installation-B"
					case "version":
						d.Version = "incompatible"
					case "socket":
						d.Socket += "-other"
					case "inode":
						d.Inode++
					case "generation":
						d.Generation = strings.Repeat("0", 24)
					case "format":
						d.Format++
					}
				})
			}
			if c, err := Dial(context.Background(), cfg); err == nil {
				c.Close()
				t.Fatal("accepted changed discovery")
			}
			if _, err := os.Lstat(l.path); err != nil {
				t.Fatalf("discovery failure removed live socket: %v", err)
			}
		})
	}
}

func TestLongRootUsesPrivateBoundRuntime(t *testing.T) {
	cfg := fixture(t)
	base := cfg.Root
	cfg.Root = filepath.Join(base, strings.Repeat("long-root-", 15))
	if err := os.Mkdir(cfg.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	// A short runtime fixture never touches another instance's runtime state.
	runtime, err := os.MkdirTemp("/tmp", "nr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	cfg.RuntimeDir = runtime
	l := listenFixture(t, cfg)
	if len(l.path) > maxSocketPath || strings.HasPrefix(l.path, cfg.Root+"/") {
		t.Fatalf("not a short socket: %s", l.path)
	}
	conn, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// Copying a discovery record to another selected root grants no access.
	other := fixture(t)
	if err := os.Mkdir(filepath.Join(other.Root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(discoveryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, discoveryPath(other), b)
	if c, err := Dial(context.Background(), other); err == nil {
		c.Close()
		t.Fatal("accepted another root's discovery")
	}
	userDir := filepath.Dir(filepath.Dir(l.path))
	if err := os.Chmod(userDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if c, err := Dial(context.Background(), cfg); err == nil {
		c.Close()
		t.Fatal("accepted exposed runtime directory")
	}
}

func TestSocketReplacementAndModesRefuse(t *testing.T) {
	for _, which := range []string{"mode", "regular", "symlink", "socket"} {
		t.Run(which, func(t *testing.T) {
			cfg := fixture(t)
			l := listenFixture(t, cfg)
			if which == "mode" {
				if err := os.Chmod(l.path, 0o660); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(l.path, l.path+"-original"); err != nil {
					t.Fatal(err)
				}
				switch which {
				case "regular":
					mustWrite(t, l.path, []byte("keep replacement"))
				case "symlink":
					if err := os.Symlink(l.path+"-original", l.path); err != nil {
						t.Fatal(err)
					}
				case "socket":
					other, err := net.ListenUnix("unix", &net.UnixAddr{Name: l.path, Net: "unix"})
					if err != nil {
						t.Fatal(err)
					}
					defer other.Close()
					if err := os.Chmod(l.path, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if c, err := Dial(context.Background(), cfg); err == nil {
				c.Close()
				t.Fatal("accepted unsafe/replaced socket")
			}
			l.Close()
			if which != "mode" {
				if _, err := os.Lstat(l.path); err != nil {
					t.Fatalf("Close removed replacement: %v", err)
				}
			}
		})
	}
}

func TestLongRootReplacementRefusesCopiedDiscovery(t *testing.T) {
	cfg := fixture(t)
	cfg.Root = filepath.Join(cfg.Root, strings.Repeat("long-root-", 15))
	if err := os.Mkdir(cfg.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := os.MkdirTemp("/tmp", "nr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	cfg.RuntimeDir = runtime
	l := listenFixture(t, cfg)
	b, err := os.ReadFile(discoveryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cfg.Root, cfg.Root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, discoveryPath(cfg), b)
	if conn, err := Dial(context.Background(), cfg); err == nil {
		conn.Close()
		t.Fatal("replacement root used original directory's socket")
	}
	if _, err := os.Lstat(l.path); err != nil {
		t.Fatal("refusal removed original socket")
	}
}

func TestIncompatibleBusyInstancePreservesLockAndDiscovery(t *testing.T) {
	cfg := fixture(t)
	l := listenFixture(t, cfg)
	lock := filepath.Join(cfg.Root, "state", "lock")
	mustWrite(t, lock, []byte("existing instance lock"))
	before, err := os.ReadFile(discoveryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version = "other-version"
	if c, err := Dial(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "incompatible instance version") {
		if c != nil {
			c.Close()
		}
		t.Fatalf("version discovery: %v", err)
	}
	if duplicate, err := Listen(cfg); !errors.Is(err, ErrBusy) {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatalf("busy incompatible listener: %v", err)
	}
	after, err := os.ReadFile(discoveryPath(cfg))
	if err != nil || string(after) != string(before) {
		t.Fatal("replaced discovery of busy instance")
	}
	b, err := os.ReadFile(lock)
	if err != nil || string(b) != "existing instance lock" {
		t.Fatal("changed instance lock")
	}
	if _, err := os.Lstat(l.path); err != nil {
		t.Fatal("removed busy instance socket")
	}
}
