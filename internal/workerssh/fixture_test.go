package workerssh

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/worker"
	"github.com/yousan/hataori/internal/workerproto"
)

var captureSSH = flag.String("capture-ssh", "", "new capture directory for this isolated OpenSSH version; existing files refuse overwrite")

type sshFixture struct {
	t                                  *testing.T
	dir, ssh, clientConfig, remoteRoot string
	user                               string
	port                               int
	server                             *exec.Cmd
	serverDone                         chan error
	resident                           *worker.Resident
	binding                            workerproto.Binding
	mu                                 sync.Mutex
	expected                           []workerproto.Target
}

func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func eventually(t *testing.T, what string, test func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for !test() {
		if time.Now().After(until) {
			t.Fatal("timed out: " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func newSSHFixture(t *testing.T, extra ...string) *sshFixture {
	t.Helper()
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("OpenSSH client is required; missing prerequisites cannot pass platform acceptance")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Fatal("ssh-keygen is required for isolated transport acceptance")
	}
	const sshd = "/usr/sbin/sshd"
	if info, err := os.Stat(sshd); err != nil || info.Mode()&0111 == 0 {
		t.Fatal("sshd is required; transport acceptance must not skip a platform")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "hws-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) // Exclusively this fixture's new data.
	f := &sshFixture{t: t, dir: dir, user: me.Username, clientConfig: filepath.Join(dir, "client.conf"), serverDone: make(chan error, 1)}
	for _, name := range []string{"host", "client", "wrong"} {
		if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "worker-fixture", "-f", filepath.Join(dir, name)).CombinedOutput(); err != nil {
			t.Fatalf("fixture keygen: %v %s", err, out)
		}
	}
	public, err := os.ReadFile(filepath.Join(dir, "client.pub"))
	if err != nil {
		t.Fatal(err)
	}
	writePrivate(t, filepath.Join(dir, "authorized_keys"), public)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config := append(extra, []string{fmt.Sprintf("ListenAddress 127.0.0.1:%d", f.port), "HostKey " + filepath.Join(dir, "host"), "AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"), "PidFile " + filepath.Join(dir, "server.pid"), "UsePAM no", "PasswordAuthentication no", "KbdInteractiveAuthentication no", "PubkeyAuthentication yes", "StrictModes no", "LogLevel ERROR", "PermitUserEnvironment no", "AllowAgentForwarding no", "X11Forwarding no"}...)
	writePrivate(t, filepath.Join(dir, "server.conf"), []byte(strings.Join(config, "\n")+"\n"))
	log, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(sshd, "-D", "-e", "-f", filepath.Join(dir, "server.conf"))
	command.Stderr = log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	f.server = command
	go func() { f.serverDone <- command.Wait() }()
	t.Cleanup(func() {
		// Only the foreground sshd PID this fixture started, never a pidfile/name.
		command.Process.Signal(syscall.SIGTERM)
		select {
		case <-f.serverDone:
		case <-time.After(2 * time.Second):
			command.Process.Kill()
			<-f.serverDone
		}
		log.Close()
	})
	eventually(t, "isolated sshd listener", func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", f.port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		return false
	})
	hostPub, err := os.ReadFile(filepath.Join(dir, "host.pub"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(hostPub))
	writePrivate(t, filepath.Join(dir, "known_hosts"), []byte(fmt.Sprintf("[127.0.0.1]:%d %s %s\n", f.port, fields[0], fields[1])))
	f.writeClient("client")
	f.ssh = filepath.Join(dir, "ssh-fixture")
	wrapper := "#!/bin/sh\nexec " + shellQuote(realSSH) + " -F " + shellQuote(f.clientConfig) + " \"$@\"\n"
	if err := os.WriteFile(f.ssh, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	// A short root exercises literal spaces, colons and backslashes in the
	// streamlocal forward, rather than always taking the hashed socket fallback.
	f.remoteRoot = filepath.Join(dir, `worker : \ space`)
	f.binding, err = worker.Initialize(f.remoteRoot, "main-fixture")
	if err != nil {
		t.Fatal(err)
	}
	resident, err := worker.Open(f.remoteRoot, f.binding, worker.Options{Config: &worker.Config{}, HostCommand: host.Command{Path: os.Args[0]}, BootID: func() (string, error) { return "boot-fixture", nil }, Telemetry: func() workerproto.Telemetry {
		return workerproto.Telemetry{ObservedMS: time.Now().UnixMilli(), ValidMS: 10000, Known: true, MemoryFree: 1024, DiskFree: 1024}
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.resident = resident
	if _, err := resident.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- resident.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		resident.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return f
}
func (f *sshFixture) writeClient(key string) {
	config := []string{"Host worker-fixture", "  HostName 127.0.0.1", fmt.Sprintf("  Port %d", f.port), "  User " + f.user, "  IdentityFile " + filepath.Join(f.dir, key), "  IdentitiesOnly yes", "  IdentityAgent none", "  UserKnownHostsFile " + filepath.Join(f.dir, "known_hosts"), "  GlobalKnownHostsFile /dev/null", "  StrictHostKeyChecking yes", "  BatchMode yes"}
	writePrivate(f.t, f.clientConfig, []byte(strings.Join(config, "\n")+"\n"))
}
func (f *sshFixture) config() Config {
	return Config{Destination: "worker-fixture", Root: f.remoteRoot, Binding: f.binding, Expected: func(context.Context) ([]workerproto.Target, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]workerproto.Target(nil), f.expected...), nil
	}}
}
func (f *sshFixture) pool(options Options) *Pool {
	f.t.Helper()
	options.RunDir = filepath.Join(f.t.TempDir(), "connections")
	options.SSH = f.ssh
	if options.ConnectTimeout == 0 {
		options.ConnectTimeout = 2 * time.Second
	}
	if options.ProbeTimeout == 0 {
		options.ProbeTimeout = 500 * time.Millisecond
	}
	if options.ProbeInterval == 0 {
		options.ProbeInterval = 200 * time.Millisecond
	}
	if options.BackoffMin == 0 {
		options.BackoffMin = 100 * time.Millisecond
	}
	if options.BackoffMax == 0 {
		options.BackoffMax = 400 * time.Millisecond
	}
	pool, err := New(context.Background(), options)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { pool.Close() })
	return pool
}
func (f *sshFixture) ready(pool *Pool, config Config) *Worker {
	f.t.Helper()
	w, err := pool.Worker(config)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		b, _ := os.ReadFile(filepath.Join(f.dir, "server.log"))
		f.t.Fatalf("readiness: %v status=%+v server=%s", err, w.Status(), b)
	}
	return w
}
func roundTrip(t *testing.T, stream *Stream, m workerproto.Message) workerproto.Message {
	t.Helper()
	stream.SetDeadline(time.Now().Add(3 * time.Second))
	if err := writeFrame(stream, m); err != nil {
		t.Fatal(err)
	}
	data, err := bufio.NewReader(stream).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	reply, err := stream.Protocol.Decode(data, m.Target)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ID != m.ID || reply.Op != m.Op {
		t.Fatalf("reply correlation: %+v", reply)
	}
	return reply
}
func request(binding workerproto.Binding, op, id string, target *workerproto.Target) workerproto.Message {
	m := workerproto.Message{Kind: "request", ID: id, Binding: binding, Op: op, Target: target, Body: json.RawMessage(`{}`)}
	if op == "prepare" {
		m.Key = "key-" + id
	}
	return m
}
