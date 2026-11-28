package workerinstall

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Build actual complete Go executables for all four targets. These are file
// formats, not invented CLI output or platform-runtime evidence.
func TestCompletePlatformBinaries(t *testing.T) {
	dir := privateTestDir(t)
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
				broken := f
				broken.binary = oversizedSegment(t, b, f.platform)
				reason := "incomplete ELF segment"
				if f.platform.OS == "darwin" {
					reason = "incomplete Mach-O segment"
				}
				if _, err := Install(prefix, broken.request(t, "incomplete", nil)); err == nil || !strings.Contains(err.Error(), reason) {
					t.Fatalf("incomplete executable was not refused by segment validation: %v", err)
				}
				if got, err := Selected(prefix, f.platform, "1.3"); err != nil || got.Path != selected.Path {
					t.Fatal("incomplete executable changed selected version")
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

func TestSignedForeignOperatingSystemKeepsSelection(t *testing.T) {
	dir := privateTestDir(t)
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newReleaseFixture(t)
	fixture.platform = Platform{"linux", "amd64"}
	build := func(goos string) []byte {
		t.Helper()
		path := filepath.Join(dir, goos)
		cmd := exec.Command("go", "build", "-p=1", "-trimpath", "-o", path, source)
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0", "GOMAXPROCS=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", goos, err, out)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	linux := build("linux")
	fixture.binary = linux
	prefix := filepath.Join(dir, "install")
	old := mustInstall(t, prefix, fixture.request(t, "v1", nil))
	foreign := build("freebsd")
	genericABI := bytes.Clone(foreign)
	genericABI[elf.EI_OSABI] = byte(elf.ELFOSABI_NONE)
	foreignABI := bytes.Clone(linux)
	foreignABI[elf.EI_OSABI] = byte(elf.ELFOSABI_FREEBSD)
	for _, b := range [][]byte{foreign, genericABI, foreignABI} {
		fixture.binary = b
		if _, err := Install(prefix, fixture.request(t, "v2", nil)); err == nil {
			t.Fatal("accepted signed foreign-OS ELF as linux-amd64")
		}
	}
	selected, err := Selected(prefix, fixture.platform, "1.3")
	if err != nil || selected.Path != old.Path {
		t.Fatalf("foreign OS changed selection: %+v %v", selected, err)
	}
}

// Alter the final segment descriptor of a real complete executable while
// keeping its exact bytes/Go build metadata. The fixture then signs those bytes
// to prove that signature/checksum alone do not establish executable completeness.
func oversizedSegment(t *testing.T, b []byte, p Platform) []byte {
	t.Helper()
	out := bytes.Clone(b)
	if p.OS == "linux" {
		e, err := elf.NewFile(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		index := -1
		for i, s := range e.Progs {
			if s.Type == elf.PT_LOAD {
				index = i
			}
		}
		if index < 0 {
			t.Fatal("real ELF fixture has no load segment")
		}
		phoff := binary.LittleEndian.Uint64(b[32:40])
		entsize := binary.LittleEndian.Uint16(b[54:56])
		offset := int(phoff) + index*int(entsize)
		binary.LittleEndian.PutUint64(out[offset+32:offset+40], uint64(len(b))+1)
		binary.LittleEndian.PutUint64(out[offset+40:offset+48], uint64(len(b))+1)
	} else {
		m, err := macho.NewFile(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		offset, last := 32, -1
		for _, load := range m.Loads {
			if _, ok := load.(*macho.Segment); ok {
				last = offset
			}
			offset += len(load.Raw())
		}
		if last < 0 {
			t.Fatal("real Mach-O fixture has no segment")
		}
		binary.LittleEndian.PutUint64(out[last+48:last+56], uint64(len(b))+1)
	}
	return out
}
