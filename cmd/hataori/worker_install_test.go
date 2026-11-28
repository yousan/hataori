package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/workerinstall"
	"github.com/yousan/hataori/internal/workerproto"
)

type workerReleaseFixture struct {
	prefix    string
	artifacts string
	manifest  string
	signature string
	trust     string
	key       ed25519.PrivateKey
	binary    []byte
}

func newWorkerReleaseFixture(t *testing.T) *workerReleaseFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &workerReleaseFixture{prefix: filepath.Join(dir, "installed"), artifacts: filepath.Join(dir, "artifacts"), manifest: filepath.Join(dir, "release.json"), signature: filepath.Join(dir, "release.sig"), trust: filepath.Join(dir, "release.pub")}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = key
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.binary, err = os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	p := workerPlatform()
	if err := os.WriteFile(filepath.Join(f.artifacts, "hataori-"+p.OS+"-"+p.Arch), f.binary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.trust, key.Public().(ed25519.PublicKey), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *workerReleaseFixture) release(t *testing.T, version string) []string {
	t.Helper()
	h := sha256.Sum256(f.binary)
	m := workerinstall.Manifest{Format: 1, Version: version, Protocol: workerproto.Version, Artifacts: []workerinstall.Artifact{{Platform: workerPlatform(), SHA256: hex.EncodeToString(h[:]), Size: int64(len(f.binary))}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.signature, ed25519.Sign(f.key, b), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--prefix", f.prefix, "--manifest", f.manifest, "--signature", f.signature, "--trust-key", f.trust, "--artifacts", f.artifacts}
}

func (f *workerReleaseFixture) install(t *testing.T, version string) workerinstall.Result {
	t.Helper()
	var out, stderr bytes.Buffer
	if code := workerInstallMain(f.release(t, version), &out, &stderr); code != 0 {
		t.Fatalf("install: exit %d: %s", code, stderr.String())
	}
	var r workerinstall.Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Version != version || r.Path == "" || r.DurabilityWarning != "" {
		t.Fatalf("install result: %s %v", out.String(), err)
	}
	return r
}

func TestWorkerInstallCommandIsExplicitAndOffline(t *testing.T) {
	f := newWorkerReleaseFixture(t)
	t.Setenv("HATAORI_ROOT", filepath.Join(t.TempDir(), "must-not-be-used"))
	var out, stderr bytes.Buffer
	if code := workerInstallMain(nil, &out, &stderr); code != exitUsage {
		t.Fatalf("missing flags: %d %s", code, stderr.String())
	}
	if _, err := os.Stat(f.prefix); !os.IsNotExist(err) {
		t.Fatal("missing flags touched prefix")
	}
	old := f.install(t, "v1")
	current := f.install(t, "v2")
	if old.Path == current.Path {
		t.Fatal("upgrade overwrote executable path")
	}
	for _, path := range []string{old.Path, current.Path} {
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, f.binary) {
			t.Fatal("installed bytes changed")
		}
	}
	if _, err := os.Stat(os.Getenv("HATAORI_ROOT")); !os.IsNotExist(err) {
		t.Fatal("installer used ambient instance root")
	}
	args := []string{"--prefix", f.prefix, "--root", filepath.Join(t.TempDir(), "worker"), "--main", "main-A", "--installation", "worker-A", "--manager"}
	manager := "systemd-user"
	if workerPlatform().OS == "darwin" {
		manager = "launch-agent"
	}
	out.Reset()
	stderr.Reset()
	if code := workerServiceTemplateMain(append(args, manager), &out, &stderr); code != 0 {
		t.Fatalf("template: %d %s", code, stderr.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(old.Path)) && !bytes.Contains(out.Bytes(), []byte(current.Path)) {
		t.Fatal("service launcher is not a retained versioned path")
	}
	if code := workerServiceTemplateMain(append(args, "auto"), &out, &stderr); code == 0 {
		t.Fatal("implicitly chose a service manager")
	}
}

func TestWorkerInstallCommandRejectsMutableTrustSource(t *testing.T) {
	for _, name := range []string{"key", "parent"} {
		t.Run(name, func(t *testing.T) {
			f := newWorkerReleaseFixture(t)
			if name == "key" {
				if err := os.Chmod(f.trust, 0o666); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(filepath.Dir(f.trust), 0o777); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := workerInstallMain(f.release(t, "v1"), &out, &stderr); code == 0 {
				t.Fatal("mutable enrollment authority accepted")
			}
			if _, err := os.Stat(f.prefix); !os.IsNotExist(err) {
				t.Fatal("unsafe enrollment touched installation prefix")
			}
		})
	}
}
