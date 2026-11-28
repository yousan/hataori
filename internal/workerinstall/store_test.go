package workerinstall

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type releaseFixture struct {
	key      ed25519.PrivateKey
	binary   []byte
	platform Platform
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return releaseFixture{key: key, binary: b, platform: Platform{runtime.GOOS, runtime.GOARCH}}
}

func (f releaseFixture) request(t *testing.T, version string, change func(*Manifest)) Request {
	t.Helper()
	h := sha256.Sum256(f.binary)
	m := Manifest{Format: 1, Version: version, Protocol: "1.3", Artifacts: []Artifact{{Platform: f.platform, SHA256: hex.EncodeToString(h[:]), Size: int64(len(f.binary))}}}
	if change != nil {
		change(&m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Manifest: b, Signature: ed25519.Sign(f.key, b), Trusted: f.key.Public().(ed25519.PublicKey), Platform: f.platform, Protocol: "1.3", Binary: bytes.NewReader(f.binary)}
}

func mustInstall(t *testing.T, path string, req Request) Result {
	t.Helper()
	r, err := Install(path, req)
	if err != nil || r.DurabilityWarning != "" {
		t.Fatalf("install: %+v %v", r, err)
	}
	return r
}

func TestInstallRetainsImmutableVersions(t *testing.T) {
	f := newReleaseFixture(t)
	prefix := filepath.Join(t.TempDir(), "install")
	old := mustInstall(t, prefix, f.request(t, "v1", nil))
	oldInfo, err := os.Stat(old.Path)
	if err != nil {
		t.Fatal(err)
	}
	current := mustInstall(t, prefix, f.request(t, "v2", nil))
	if old.Path == current.Path {
		t.Fatal("overwrote old executable name")
	}
	info, err := os.Stat(old.Path)
	if err != nil || !os.SameFile(oldInfo, info) {
		t.Fatalf("old executable replaced: %v", err)
	}
	b, err := os.ReadFile(old.Path)
	if err != nil || !bytes.Equal(b, f.binary) {
		t.Fatal("old executable modified")
	}
	selected, err := Selected(prefix, f.platform, "1.3")
	if err != nil || selected.Path != current.Path {
		t.Fatalf("selection: %+v %v", selected, err)
	}
	// Re-selecting a retained verified version is explicit manual recovery;
	// neither it nor a repeated install rewrites the executable inode.
	again := mustInstall(t, prefix, f.request(t, "v1", nil))
	info, err = os.Stat(again.Path)
	if err != nil || !os.SameFile(oldInfo, info) {
		t.Fatal("reinstall rewrote old executable")
	}
	if _, err := os.Stat(current.Path); err != nil {
		t.Fatal("rollback removed newer version")
	}
}

type brokenReader struct{ io.Reader }

func (r brokenReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if err == io.EOF {
		return n, errors.New("fixture download interrupted")
	}
	return n, err
}

func TestInstallFailuresPreserveSelectionAndVersions(t *testing.T) {
	f := newReleaseFixture(t)
	for _, name := range []string{"checksum", "partial", "oversize", "size_mismatch", "reader_failure", "signature", "trust_key", "protocol", "platform", "binary_platform", "binary_format", "publish_failure", "selector_failure", "busy"} {
		t.Run(name, func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "install")
			old := mustInstall(t, prefix, f.request(t, "v1", nil))
			before, err := os.ReadFile(filepath.Join(prefix, "selected.json"))
			if err != nil {
				t.Fatal(err)
			}
			req := f.request(t, "v2", nil)
			switch name {
			case "checksum":
				b := bytes.Clone(f.binary)
				b[4096] ^= 1
				req.Binary = bytes.NewReader(b)
			case "partial":
				req.Binary = bytes.NewReader(f.binary[:len(f.binary)/2])
			case "oversize":
				req.Binary = io.MultiReader(bytes.NewReader(f.binary), bytes.NewReader([]byte{1}))
			case "size_mismatch":
				req = f.request(t, "v2", func(m *Manifest) { m.Artifacts[0].Size++ })
			case "reader_failure":
				req.Binary = brokenReader{bytes.NewReader(f.binary)}
			case "signature":
				req.Signature = make([]byte, 64)
			case "trust_key":
				other := newReleaseFixture(t)
				req = other.request(t, "v2", nil)
			case "protocol":
				req = f.request(t, "v2", func(m *Manifest) { m.Protocol = "2.0" })
			case "platform":
				req.Platform.OS = "windows"
			case "binary_platform":
				req = f.request(t, "v2", func(m *Manifest) {
					if m.Artifacts[0].OS == "linux" {
						m.Artifacts[0].OS = "darwin"
					} else {
						m.Artifacts[0].OS = "linux"
					}
				})
				req.Platform.OS = "linux"
				if f.platform.OS == "linux" {
					req.Platform.OS = "darwin"
				}
			case "binary_format":
				other := f
				other.binary = []byte("#!/bin/sh\nexit 0\n")
				req = other.request(t, "v2", nil)
			case "publish_failure":
				m, a, e := Verify(req.Manifest, req.Signature, req.Trusted, req.Platform, req.Protocol)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.Mkdir(filepath.Join(prefix, binaryName(m, a)), 0o700); e != nil {
					t.Fatal(e)
				}
			case "selector_failure": // A directory in place of our selector is refused, never replaced.
				if err := os.Rename(filepath.Join(prefix, "selected.json"), filepath.Join(prefix, "saved.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(prefix, "selected.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "busy":
				s, e := openStore(prefix, req.Trusted, false)
				if e != nil {
					t.Fatal(e)
				}
				defer s.close()
			}
			if _, err = Install(prefix, req); err == nil {
				t.Fatal("unsafe installation accepted")
			}
			if name == "selector_failure" {
				if err = os.Remove(filepath.Join(prefix, "selected.json")); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(filepath.Join(prefix, "saved.json"), filepath.Join(prefix, "selected.json")); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(filepath.Join(prefix, "selected.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed selection")
			}
			b, err := os.ReadFile(old.Path)
			if err != nil || !bytes.Equal(b, f.binary) {
				t.Fatal("refusal changed old executable")
			}
		})
	}
}

func TestInstallRefusesAdoptionAndLinks(t *testing.T) {
	f := newReleaseFixture(t)
	t.Run("unregistered", func(t *testing.T) {
		prefix := t.TempDir()
		if err := os.Chmod(prefix, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(prefix, "work")
		if err := os.WriteFile(marker, []byte("only copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(prefix, f.request(t, "v1", nil)); err == nil || !strings.Contains(err.Error(), "refusing nonempty unregistered prefix") {
			t.Fatalf("nonempty prefix was not refused before adoption: %v", err)
		}
		b, err := os.ReadFile(marker)
		if err != nil || string(b) != "only copy" {
			t.Fatal("lost existing work")
		}
	})
	t.Run("prefix_symlink", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(link, f.request(t, "v1", nil)); err == nil {
			t.Fatal("followed symlinked prefix")
		}
		entries, err := os.ReadDir(target)
		if err != nil || len(entries) != 0 {
			t.Fatal("wrote through prefix link")
		}
	})
	for _, name := range []string{"trust-key", "versions", "selected.json", "install.lock"} {
		t.Run(name, func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "install")
			mustInstall(t, prefix, f.request(t, "v1", nil))
			target := filepath.Join(prefix, "unrelated")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(prefix, name), filepath.Join(prefix, "saved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(prefix, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(prefix, f.request(t, "v2", nil)); err == nil {
				t.Fatal("followed or replaced private symlink")
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "keep" {
				t.Fatal("modified link target")
			}
		})
	}
}

func TestSelectedRefusesChangedBinary(t *testing.T) {
	f := newReleaseFixture(t)
	prefix := filepath.Join(t.TempDir(), "install")
	old := mustInstall(t, prefix, f.request(t, "v1", nil))
	if err := os.Chmod(old.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old.Path, []byte("incomplete"), 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := Selected(prefix, f.platform, "1.3"); err == nil {
		t.Fatal("selected modified executable")
	}
	if _, err := Install(prefix, f.request(t, "v2", nil)); err == nil {
		t.Fatal("ignored broken existing selection")
	}
}

func TestSelectionIsAlwaysCompleteAndAlreadyPublished(t *testing.T) {
	f := newReleaseFixture(t)
	prefix := filepath.Join(t.TempDir(), "install")
	mustInstall(t, prefix, f.request(t, "v0", nil))
	stop := make(chan struct{})
	result := make(chan error, 1)
	var ready sync.WaitGroup
	ready.Add(1)
	go func() {
		ready.Done()
		for {
			select {
			case <-stop:
				result <- nil
				return
			default:
			}
			b, err := os.ReadFile(filepath.Join(prefix, "selected.json"))
			if err != nil {
				result <- err
				return
			}
			var sel Selection
			if err := json.Unmarshal(b, &sel); err != nil {
				result <- fmt.Errorf("torn selection: %w", err)
				return
			}
			m, a, err := Verify(sel.Manifest, sel.Signature, f.key.Public().(ed25519.PublicKey), f.platform, "1.3")
			if err != nil {
				result <- err
				return
			}
			info, err := os.Stat(filepath.Join(prefix, binaryName(m, a)))
			if err != nil || info.Size() != a.Size || info.Mode().Perm() != 0o500 {
				result <- fmt.Errorf("selection preceded complete executable publication: %v", err)
				return
			}
		}
	}()
	ready.Wait()
	for n := 1; n <= 4; n++ {
		mustInstall(t, prefix, f.request(t, fmt.Sprintf("v%d", n), nil))
	}
	close(stop)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
