package nativehttp

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This fixture requires a test runner already authorized to change UID.
// It never invokes sudo, changes a host account or modifies a service. A
// skip is explicitly not cross-user acceptance on that platform.
func TestNativeCrossUserBoundary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires an isolated privileged test runner; no sudo or host account changes")
	}
	cfg := fixture(t)
	binaryDir := fixture(t).Root
	if err := os.Chmod(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binaryDir, "peer-test")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(binary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	l := listenFixture(t, cfg)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("accepted")) })}
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	run := func(mode string) {
		t.Helper()
		cmd := exec.Command(binary, "-test.run=^TestNativeCrossUserHelper$", "-test.count=1")
		cmd.Env = []string{"NATIVE_HTTP_CROSSUSER_SOCKET=" + l.path, "NATIVE_HTTP_CROSSUSER_MODE=" + mode}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}}
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("foreign helper: %v\n%s", err, b)
		}
	}
	run("private")
	// Deliberately expose only the disposable fixture so the kernel peer
	// guard is exercised separately from directory/socket permissions.
	for _, path := range []string{cfg.Root, filepath.Join(cfg.Root, "state")} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(l.path, 0o666); err != nil {
		t.Fatal(err)
	}
	run("peer")
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func TestNativeCrossUserHelper(t *testing.T) {
	path := os.Getenv("NATIVE_HTTP_CROSSUSER_SOCKET")
	if path == "" {
		t.Skip("subprocess helper")
	}
	if os.Geteuid() != 65534 {
		t.Fatal("fixture did not change OS user")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if os.Getenv("NATIVE_HTTP_CROSSUSER_MODE") == "private" {
		if !errors.Is(err, syscall.EACCES) {
			if conn != nil {
				conn.Close()
			}
			t.Fatalf("private socket: %v; want permission denied", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: instance\r\n\r\n")); err != nil {
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
			return
		}
		t.Fatalf("no definitive peer refusal on write: %v", err)
	}
	var b [1]byte
	n, readErr := conn.Read(b[:])
	if n != 0 {
		t.Fatal("foreign peer reached HTTP")
	}
	if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("no definitive peer refusal: %v", readErr)
	}
}
