package workerssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCapturedOpenSSHFailures(t *testing.T) {
	for _, cause := range []Cause{HostKey, Authentication, Forward} {
		t.Run(string(cause), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "openssh-9.6p1", string(cause)+".stderr"))
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic diagnostic
			// Exercise real stderr split across reads, including the bounded tail.
			diagnostic.Write(bytes.Repeat([]byte("x"), 9000))
			for _, b := range data {
				diagnostic.Write([]byte{b})
			}
			if got := diagnostic.cause(Transport); got != cause {
				t.Fatalf("classification: %s", got)
			}
			if len(diagnostic.snapshot()) > 8192 {
				t.Fatal("unbounded SSH output")
			}
			if got := unavailable(cause).Error(); got != "worker SSH unavailable: "+string(cause) {
				t.Fatal("raw SSH output escaped", got)
			}
		})
	}
}

func TestDestinationOwnershipAndRegistration(t *testing.T) {
	f := newSSHFixture(t)
	pool := f.pool(Options{})
	w := f.ready(pool, f.config())
	conflict := f.config()
	conflict.Binding.Worker = "other-worker"
	if _, err := pool.Worker(conflict); err == nil {
		t.Fatal("same destination accepted another installation")
	}
	duplicate, err := New(context.Background(), pool.options)
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	if _, err := duplicate.Worker(f.config()); err == nil {
		t.Fatal("duplicate pool owned the same SSH destination")
	}
	if !w.Status().Ready {
		t.Fatal("ownership check disturbed original connection")
	}
	bad := f.config()
	bad.Destination = "-oProxyCommand=anything"
	if _, err := pool.Worker(bad); err == nil {
		t.Fatal("destination enabled option injection")
	}
	// A private-directory lock must never follow an existing symlink.
	other := f.config()
	other.Destination = "other-fixture"
	sum := sha256.Sum256([]byte(other.Destination))
	target := filepath.Join(f.dir, "untouched-lock-target")
	writePrivate(t, target, []byte("keep"))
	if err := os.Symlink(target, filepath.Join(pool.options.RunDir, hex.EncodeToString(sum[:])+".lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Worker(other); err == nil {
		t.Fatal("lock followed symlink")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Fatal("lock target modified", err)
	}
}

func TestCleanupPreservesReplacementsAndUnknownFiles(t *testing.T) {
	for _, replacement := range []string{"regular", "socket"} {
		t.Run(replacement, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "attempt")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "forward.sock")
			original, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			original.SetUnlinkOnClose(false)
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			g := &generation{dir: dir, dirInfo: info, sockets: map[string]os.FileInfo{}}
			if !g.noteSocket(path) {
				t.Fatal("could not record own socket")
			}
			original.Close()
			// Keep the original inode allocated so the test cannot get a reused inode.
			if err := os.Rename(path, filepath.Join(dir, "original")); err != nil {
				t.Fatal(err)
			}
			if replacement == "regular" {
				writePrivate(t, path, []byte("keep"))
			} else {
				replaced, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				replaced.SetUnlinkOnClose(false)
				defer replaced.Close()
			}
			writePrivate(t, filepath.Join(dir, "unknown"), []byte("keep"))
			g.cleanup()
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("replacement removed", err)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "unknown")); err != nil || string(data) != "keep" {
				t.Fatal("unknown file removed", err)
			}
		})
	}
}
