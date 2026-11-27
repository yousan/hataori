package workerssh

import (
	"bytes"
	"context"
	"github.com/yousan/hataori/internal/workerproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHArgumentsAndCredentialFiles(t *testing.T) {
	f := newSSHFixture(t)
	// Trust the hostname only. CheckHostIP=yes would append its new IP despite
	// strict checking; unsafe config defaults must be overridden by the caller.
	known, err := os.ReadFile(filepath.Join(f.dir, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	known = bytes.ReplaceAll(known, []byte("127.0.0.1"), []byte("localhost"))
	writePrivate(t, filepath.Join(f.dir, "known_hosts"), known)
	config, err := os.ReadFile(f.clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.ReplaceAll(config, []byte("HostName 127.0.0.1"), []byte("HostName localhost\n  AddressFamily inet"))
	marker := filepath.Join(f.dir, "forbidden-command")
	config = append(config, []byte("  CheckHostIP yes\n  ForwardAgent yes\n  ForwardX11 yes\n  ForwardX11Trusted yes\n  PermitLocalCommand yes\n  LocalCommand touch "+marker+"\n  RemoteCommand touch "+marker+"\n  ForkAfterAuthentication yes\n  RemoteForward 0 127.0.0.1:1\n")...)
	writePrivate(t, f.clientConfig, config)
	before := map[string][]byte{}
	for _, name := range []string{"known_hosts", "client", "client.pub", "wrong", "wrong.pub", "host", "host.pub", "authorized_keys", "client.conf"} {
		data, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	args := masterArgs("private-control", "worker-fixture")
	joined := " " + strings.Join(args, " ") + " "
	for _, option := range []string{"ControlPersist=no", "ForkAfterAuthentication=no", "ClearAllForwardings=yes", "BatchMode=yes", "StrictHostKeyChecking=yes", "UpdateHostKeys=no", "CheckHostIP=no", "ForwardAgent=no", "ForwardX11=no", "ForwardX11Trusted=no", "PermitLocalCommand=no", "RemoteCommand=none", "GSSAPIDelegateCredentials=no", "AddKeysToAgent=no", "PasswordAuthentication=no", "KbdInteractiveAuthentication=no"} {
		if !strings.Contains(joined, " -o "+option+" ") {
			t.Fatal("missing protection", option)
		}
	}
	for _, arg := range args {
		if arg == "-R" || arg == "-L" || arg == "-A" || arg == "-X" || arg == "-Y" {
			t.Fatal("unrequested forward", arg)
		}
	}
	if !strings.Contains(joined, " -N -n -T ") {
		t.Fatal("remote session/terminal was enabled")
	}
	mux := strings.Join(muxArgs("private-control", "worker-fixture", "forward", "-L", "local:resident"), " ")
	if !strings.Contains(mux, "-F /dev/null") || !strings.Contains(mux, "-O forward") || strings.Contains(mux, "-R") {
		t.Fatal("unsafe mux command", mux)
	}
	pool := f.pool(Options{})
	w := f.ready(pool, f.config())
	if !w.Status().Ready {
		t.Fatal(w.Status())
	}
	w.mu.Lock()
	attempt := w.current.dir
	w.mu.Unlock()
	entries, err := os.ReadDir(attempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "control.sock" && entry.Name() != "resident.sock" {
			t.Fatal("unexpected file written by transport", entry.Name())
		}
	}
	entries, err = os.ReadDir(pool.options.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".lock") {
			t.Fatal("unexpected connection state", entry.Name())
		}
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatal("operator credential/config was changed", name, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("configured local/remote command ran", err)
	}
	t.Log("ran: unsafe fixture config overridden; no remote command/reverse forward, no credential/config writes; hostname-only known_hosts unchanged")
}

// The helper's child intentionally inherits stderr, then releases it when the
// test writes a private file. Neither the product nor the test kills that child.
func TestSSHStderrHelper(t *testing.T) {
	role := os.Getenv("HATAORI_SSH_TEST_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("HATAORI_SSH_TEST_DIR")
	if role == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestSSHStderrHelper$")
		child.Env = append(os.Environ(), "HATAORI_SSH_TEST_ROLE=child")
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(2)
		}
		if os.WriteFile(filepath.Join(dir, "started"), []byte("yes"), 0600) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	if role == "child" {
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
		os.Exit(0)
	}
	os.Exit(2)
}
func TestInheritedStderrCannotHangClose(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "ssh-helper")
	script := "#!/bin/sh\nHATAORI_SSH_TEST_ROLE=parent HATAORI_SSH_TEST_DIR=" + shellQuote(dir) + " exec " + shellQuote(os.Args[0]) + " -test.run='^TestSSHStderrHelper$'\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writePrivate(t, filepath.Join(dir, "release"), []byte("yes")) })
	pool, err := New(context.Background(), Options{RunDir: filepath.Join(dir, "locks"), SSH: wrapper, ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Worker(Config{Destination: "worker-fixture", Root: filepath.Join(dir, "resident"), Binding: workerproto.Binding{Main: "main-fixture", Worker: "worker-fixture"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "own stderr-inheriting helper started", func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })
	done := make(chan struct{})
	go func() { pool.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		writePrivate(t, filepath.Join(dir, "release"), []byte("yes"))
		t.Fatal("Close waited for an unowned stderr-inheriting helper")
	}
	t.Log("ran: owned SSH process exits while helper holds stderr; Close bounded without signalling helper")
}
