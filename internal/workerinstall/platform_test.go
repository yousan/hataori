package workerinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Build actual complete Go executables for all four targets. These are file
// formats, not invented CLI output or platform-runtime evidence.
func TestCompletePlatformBinaries(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newReleaseFixture(t)
	for _, goos := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(goos+"-"+arch, func(t *testing.T) {
				path := filepath.Join(dir, goos+"-"+arch)
				cmd := exec.Command("go", "build", "-p=1", "-trimpath", "-o", path, source)
				cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0", "GOMAXPROCS=1")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("build: %v %s", err, out)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				f := fixture
				f.binary = b
				f.platform = Platform{goos, arch}
				prefix := filepath.Join(dir, "install-"+goos+"-"+arch)
				selected := mustInstall(t, prefix, f.request(t, "v1", nil))
				if got, err := Selected(prefix, f.platform, "1.3"); err != nil || got.Path != selected.Path {
					t.Fatalf("selected: %+v %v", got, err)
				}
				// Correctly signed metadata must not turn the wrong executable
				// architecture into an acceptable artifact.
				if arch == "amd64" {
					f.platform.Arch = "arm64"
				} else {
					f.platform.Arch = "amd64"
				}
				wrongPrefix := filepath.Join(dir, "wrong-"+goos+"-"+arch)
				if _, err := Install(wrongPrefix, f.request(t, "v1", nil)); err == nil {
					t.Fatal("accepted wrong architecture")
				}
				if _, err := os.Stat(filepath.Join(wrongPrefix, "selected.json")); !os.IsNotExist(err) {
					t.Fatal("wrong-platform install selected a binary")
				}
			})
		}
	}
}
