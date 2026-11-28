package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/yousan/hataori/internal/workerinstall"
	"github.com/yousan/hataori/internal/workerproto"
	"golang.org/x/sys/unix"
)

func workerPlatform() workerinstall.Platform {
	return workerinstall.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

func workerInstallMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker install|upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("prefix", "", "dedicated binary installation prefix, separate from the worker root")
	manifest := fs.String("manifest", "", "signed release manifest file")
	signature := fs.String("signature", "", "raw detached Ed25519 signature file")
	trust := fs.String("trust-key", "", "independently trusted raw Ed25519 public key file (32 bytes)")
	artifacts := fs.String("artifacts", "", "directory containing hataori-OS-ARCH release executables")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *prefix == "" || *manifest == "" || *signature == "" || *trust == "" || *artifacts == "" {
		return exitUsage
	}
	raw, err := readInstallInput(*manifest, workerinstall.MaxManifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	sig, err := readInstallInput(*signature, ed25519.SignatureSize)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	key, err := readInstallInput(*trust, ed25519.PublicKeySize)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	p := workerPlatform()
	if _, _, err := workerinstall.Verify(raw, sig, key, p, workerproto.Version); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	f, err := openInstallInput(filepath.Join(*artifacts, "hataori-"+p.OS+"-"+p.Arch))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer f.Close()
	result, err := workerinstall.Install(*prefix, workerinstall.Request{Manifest: raw, Signature: sig, Trusted: key, Platform: p, Protocol: workerproto.Version, Binary: f})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintln(stderr, "worker install: selection committed; could not write result")
		return exitError
	}
	return 0
}

func openInstallInput(path string) (*os.File, error) {
	i, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() {
		return nil, errors.New("worker install: input must be a regular file, not a link or device")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(i, opened) {
		f.Close()
		return nil, errors.New("worker install: input identity changed")
	}
	return f, nil
}

func readInstallInput(path string, max int64) ([]byte, error) {
	f, err := openInstallInput(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("worker install: input exceeds its limit or is unreadable")
	}
	return b, nil
}

func workerRunSelected(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker run-selected", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("prefix", "", "dedicated binary installation prefix")
	root := fs.String("root", "", "worker root")
	main := fs.String("main", "", "registered main installation identity")
	installation := fs.String("installation", "", "worker installation identity")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *prefix == "" || *root == "" || *main == "" || *installation == "" {
		return exitUsage
	}
	selected, err := workerinstall.Selected(*prefix, workerPlatform(), workerproto.Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	argv := []string{selected.Path, "worker", "run", "--root", *root, "--main", *main, "--installation", *installation}
	// Replace only this explicitly invoked launcher. The immutable selected
	// path becomes os.Executable(), pinning future host and hook launches.
	if err := syscall.Exec(selected.Path, argv, os.Environ()); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return 0
}

func workerServiceTemplateMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker service-template", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("prefix", "", "dedicated binary installation prefix")
	manager := fs.String("manager", "", "explicit systemd-user or launch-agent selection; renders only")
	root := fs.String("root", "", "worker root")
	main := fs.String("main", "", "registered main installation identity")
	installation := fs.String("installation", "", "worker installation identity")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *prefix == "" || *manager == "" || *root == "" || *main == "" || *installation == "" {
		return exitUsage
	}
	selected, err := workerinstall.Selected(*prefix, workerPlatform(), workerproto.Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	b, err := workerinstall.ServiceTemplate(workerinstall.Service{Platform: workerPlatform(), Manager: *manager, Launcher: selected.Path, Prefix: selected.Prefix, Root: *root, Main: *main, Installation: *installation})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if _, err := stdout.Write(b); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return 0
}
