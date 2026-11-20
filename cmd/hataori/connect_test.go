package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// sshHost is an sshd of the test's own: run as this user, on a loopback
// port, with its own host key and authorized_keys in a directory of its
// own, and an ssh that reaches it as the host "test" and reads none of the
// user's SSH files (its config is given with -F, the agent is not used).
type sshHost struct {
	dir  string
	ssh  string // a wrapper on PATH as "ssh": it records each call, then runs the real ssh
	log  string // the wrapper's record: each call's arguments, one per line, then "---"
	port int
}

// shortDir is a new directory with a short path, for Unix sockets, removed
// when the test ends.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "hc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// startSSHD starts the test's own sshd, with the lines of extra in its
// config. Without an sshd the test is skipped, except in CI.
func startSSHD(t *testing.T, extra ...string) *sshHost {
	t.Helper()
	sshd := "/usr/sbin/sshd"
	realSSH, err := exec.LookPath("ssh")
	if _, serr := os.Stat(sshd); serr != nil || err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("no sshd or ssh to test hataori connect against: %v %v", serr, err)
		}
		t.Skip("no sshd or ssh")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	d := shortDir(t)
	for _, k := range []string{"host", "user"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(d, k)).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
	}
	pub, _ := os.ReadFile(filepath.Join(d, "user.pub"))
	os.WriteFile(filepath.Join(d, "authorized_keys"), pub, 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := []string{
		fmt.Sprintf("ListenAddress 127.0.0.1:%d", port),
		"HostKey " + filepath.Join(d, "host"),
		"AuthorizedKeysFile " + filepath.Join(d, "authorized_keys"),
		"PidFile " + filepath.Join(d, "sshd.pid"),
		"UsePAM no", "PasswordAuthentication no", "KbdInteractiveAuthentication no", "PubkeyAuthentication yes",
		"StrictModes no", "LogLevel ERROR",
	}
	cfg = append(extra, cfg...) // the first value of a keyword counts
	os.WriteFile(filepath.Join(d, "sshd_config"), []byte(strings.Join(cfg, "\n")+"\n"), 0o600)
	if out, err := exec.Command(sshd, "-f", filepath.Join(d, "sshd_config"), "-E", filepath.Join(d, "sshd.log")).CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(d, "sshd.log"))
		t.Fatalf("sshd: %v %s %s", err, out, log)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(d, "sshd.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				syscall.Kill(pid, syscall.SIGTERM)
			}
		}
	})
	hostPub, _ := os.ReadFile(filepath.Join(d, "host.pub"))
	f := strings.Fields(string(hostPub))
	os.WriteFile(filepath.Join(d, "known_hosts"), []byte(fmt.Sprintf("[127.0.0.1]:%d %s %s\n", port, f[0], f[1])), 0o600)
	client := strings.Join([]string{
		"Host test",
		"  HostName 127.0.0.1",
		fmt.Sprintf("  Port %d", port),
		"  User " + me.Username,
		"  IdentityFile " + filepath.Join(d, "user"),
		"  IdentitiesOnly yes",
		"  UserKnownHostsFile " + filepath.Join(d, "known_hosts"),
		"  StrictHostKeyChecking yes",
		"  BatchMode yes",
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(d, "ssh_config"), []byte(client), 0o600)
	bin := filepath.Join(d, "bin")
	os.Mkdir(bin, 0o700)
	h := &sshHost{dir: d, ssh: filepath.Join(bin, "ssh"), log: filepath.Join(d, "calls"), port: port}
	// Each call's record reaches the log in one write: printf writes each
	// of its arguments on its own, so two calls at once, as two connects
	// started together make, would interleave their lines (#478). The
	// record is put together in a file of its own, and cat appends it.
	wrapper := fmt.Sprintf("#!/bin/sh\nr=$(mktemp %s) && printf '%%s\\n' \"$@\" '---' > \"$r\" && cat \"$r\" >> %s\nrm -f \"$r\"\nexec env -u SSH_AUTH_SOCK %s -F %s \"$@\"\n",
		quoteSh(filepath.Join(d, "call.XXXXXX")), quoteSh(h.log), quoteSh(realSSH), quoteSh(filepath.Join(d, "ssh_config")))
	os.WriteFile(h.ssh, []byte(wrapper), 0o700)
	eventually(t, "the test's sshd to accept a login", 10*time.Second, func() bool {
		return exec.Command(h.ssh, "test", "true").Run() == nil
	})
	os.WriteFile(h.log, nil, 0o600) // the check above is not hataori's
	return h
}

func quoteSh(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// calls is every ssh call recorded so far, each its arguments.
func (h *sshHost) calls(t *testing.T) [][]string {
	t.Helper()
	b, _ := os.ReadFile(h.log)
	var out [][]string
	var cur []string
	for _, l := range strings.Split(string(b), "\n") {
		if l == "---" {
			out = append(out, cur)
			cur = nil
			continue
		}
		if l != "" || cur != nil {
			cur = append(cur, l)
		}
	}
	return out
}

// launcher is a stand-in for the browser's launcher: it records each link
// it is given, one per line, in a file of the test's own.
type launcher struct {
	path, record string
}

// It then stays, as xdg-open does when it runs the browser in the
// foreground, until the test ends: connect must not wait for it.
func newLauncher(t *testing.T) *launcher {
	t.Helper()
	d := shortDir(t)
	l := &launcher{path: filepath.Join(d, "open"), record: filepath.Join(d, "opened")}
	done := filepath.Join(d, "done")
	os.WriteFile(l.path, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$1\" >> %s\ni=0\nwhile [ ! -e %s ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done\n",
		quoteSh(l.record), quoteSh(done))), 0o700)
	t.Cleanup(func() { os.WriteFile(done, nil, 0o600) })
	return l
}

// opened waits for the nth link the launcher was given and returns it.
func (l *launcher) opened(t *testing.T, n int) string {
	t.Helper()
	var links []string
	eventually(t, fmt.Sprintf("link %d to be opened", n), 30*time.Second, func() bool {
		b, _ := os.ReadFile(l.record)
		links = strings.Fields(string(b))
		return len(links) >= n
	})
	return links[n-1]
}

// connectRun is one `hataori connect` running in the test.
type connectRun struct {
	cancel   context.CancelFunc
	finished chan struct{}
	code     int // once finished
	mu       sync.Mutex
	out      strings.Builder
}

func (r *connectRun) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.Write(p)
}

func (r *connectRun) said() string { r.mu.Lock(); defer r.mu.Unlock(); return r.out.String() }

// stop stops the run and returns its exit code.
func (r *connectRun) stop(t *testing.T) int {
	t.Helper()
	r.cancel()
	select {
	case <-r.finished:
		return r.code
	case <-time.After(30 * time.Second):
		t.Fatalf("hataori connect did not stop:\n%s", r.said())
	}
	return -1
}

// wait waits for the run to end by itself and returns its exit code.
func (r *connectRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.finished:
		return r.code
	case <-time.After(60 * time.Second):
		t.Fatalf("hataori connect did not end:\n%s", r.said())
	}
	return -1
}

// startConnect runs `hataori connect` with args and env until stopped.
func startConnect(t *testing.T, env connectEnv, args ...string) *connectRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &connectRun{cancel: cancel, finished: make(chan struct{})}
	go func() { r.code = connectMain(ctx, args, r, env); close(r.finished) }()
	t.Cleanup(func() { cancel(); <-r.finished })
	return r
}

// sessionTokenOf is the token in a sign-in link's fragment.
func sessionTokenOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := url.ParseQuery(u.Fragment)
	return v.Get("t")
}

// The whole of hataori connect against a real sshd and a real daemon: it
// forwards a port of another number, opens the browser at the link with
// that port, and the page signs in through it; the ssh it runs gets only
// the control socket, the forward and the keep-alive; the link is in no
// file, no log and no ssh call; the host, the root and the port are
// remembered; run again, it has the running one open a fresh link; when
// the connection is cut it connects again on the same port, and the event
// stream goes on with no gap and no repeat.
func TestConnect(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	stopDaemon := runDaemon(t, root)
	startThread(t, root, "t1", "reply over ssh")
	h := startSSHD(t)
	l := newLauncher(t)
	env := connectEnv{SSH: h.ssh, Open: l.path, StateFile: filepath.Join(shortDir(t), "cfg", "connect.json"), RunDir: filepath.Join(shortDir(t), "run")}
	r := startConnect(t, env, "test", "--root", root, "--hataori", os.Args[0])

	link := l.opened(t, 1)
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "http" || u.Path != "/signin" || u.RawQuery != "" || sessionTokenOf(t, link) == "" {
		t.Fatalf("opened %q", link)
	}
	h1, lp, _ := net.SplitHostPort(u.Host)
	daemonLink := signinLink(t, root)
	dl, _ := url.Parse(daemonLink)
	if _, dp, _ := net.SplitHostPort(dl.Host); h1 != "127.0.0.1" || lp == dp {
		t.Fatalf("opened %s: not 127.0.0.1 with a port of its own (the daemon's is %s)", u.Host, dp)
	}
	// It said where the dashboard is soon after the launcher started, which
	// stays until the test ends: it did not wait for it.
	eventually(t, "it to say where the dashboard is", 10*time.Second, func() bool {
		return strings.Contains(r.said(), "http://127.0.0.1:"+lp)
	})
	session := openLink(t, link)
	base := "http://127.0.0.1:" + lp
	if code, body := dashGet(t, base, session, "/api/threads"); code != http.StatusOK || !strings.Contains(body, "reply over ssh") {
		t.Fatalf("threads through the forward: %d %s", code, body)
	}

	// What it ran: only the control socket, the forward and the keep-alive,
	// and never the link.
	checkCalls(t, h, env.RunDir, root, dl.Host, sessionTokenOf(t, link))

	// The run directory is the person's alone, and holds sockets only.
	fi, err := os.Stat(env.RunDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the run directory: %v %v", fi.Mode(), err)
	}
	entries, _ := os.ReadDir(env.RunDir)
	for _, e := range entries {
		if e.Type()&os.ModeSocket == 0 && !strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("the run directory holds %s, which is neither a socket nor the lock", e.Name())
		}
	}
	// The host is remembered, and the link is not.
	b, _ := os.ReadFile(env.StateFile)
	var st struct {
		Last  string
		Hosts map[string]struct {
			Root, Hataori string
			Port          int
		}
	}
	if err := json.Unmarshal(b, &st); err != nil || st.Last != "test" || st.Hosts["test"].Root != root ||
		st.Hosts["test"].Hataori != os.Args[0] || strconv.Itoa(st.Hosts["test"].Port) != lp {
		t.Fatalf("remembered: %s, %v", b, err)
	}
	if strings.Contains(string(b), sessionTokenOf(t, link)) {
		t.Fatal("the link is in the remembered hosts")
	}
	if fi, _ := os.Stat(env.StateFile); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the remembered hosts are readable by others: %v", fi.Mode())
	}
	cfgFiles, _ := os.ReadDir(filepath.Dir(env.StateFile))
	for _, f := range cfgFiles {
		if f.Name() != filepath.Base(env.StateFile) && f.Name() != filepath.Base(env.StateFile)+".lock" {
			t.Fatalf("the configuration directory holds %s", f.Name())
		}
	}

	// Run again, with no argument: the running one opens a fresh link,
	// though something holds a connection to its socket and says nothing.
	socks, _ := filepath.Glob(filepath.Join(env.RunDir, "*.sock"))
	if len(socks) != 1 {
		t.Fatalf("sockets for second runs: %v", socks)
	}
	idle, err := net.Dial("unix", socks[0])
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	began := time.Now()
	second := startConnect(t, env)
	if code := second.wait(t); code != 0 {
		t.Fatalf("the second connect: exit %d:\n%s", code, second.said())
	}
	if d := time.Since(began); d > 20*time.Second {
		t.Fatalf("the second connect took %v: a silent connection held the first one up", d)
	}
	fresh := l.opened(t, 2)
	if fresh == link || !strings.HasPrefix(fresh, "http://127.0.0.1:"+lp+"/signin#t=") {
		t.Fatalf("the fresh link: %q", fresh)
	}
	openLink(t, fresh)

	// Cut the connection: it connects again on the same port, and the
	// event stream, resumed, misses nothing and repeats nothing.
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	before := readAllEvents(t, c)
	events, stopStream := dashStream(t, base, session, 0)
	seen := readUntilCaughtUp(t, events)
	stopStream()
	killMaster(t, h, env.RunDir)
	// While it connects again, it keeps the port: nobody else can take it.
	if ln, err := net.Listen("tcp", "127.0.0.1:"+lp); err == nil {
		ln.Close()
		t.Fatal("the local port was free while the connection was down")
	}
	eventually(t, "the connection to drop and come back", 60*time.Second, func() bool {
		return strings.Contains(r.said(), "connected to test again")
	})
	startThread(t, root, "t2", "reply after the cut")
	eventually(t, "the second thread's turn to end", 20*time.Second, func() bool {
		ts, err := c.Threads(context.Background(), control.Filter{})
		return err == nil && len(ts) == 2 && ts[0].State == control.Idle && ts[1].State == control.Idle &&
			len(readAllEvents(t, c)) > len(before)
	})
	events, stopStream = dashStream(t, base, session, seen[len(seen)-1])
	seen = append(seen, readUntilCaughtUp(t, events)...)
	stopStream()
	if all := readAllEvents(t, c); !slices.Equal(seen, all) {
		t.Fatalf("through the reconnected port: events %v, want %v", seen, all)
	}
	if code, _ := dashGet(t, base, session, "/api/threads"); code != http.StatusOK {
		t.Fatalf("the session after the cut: %d", code)
	}

	// The daemon comes back on another port, and the connection drops: it
	// learns the new address when it connects again.
	c.Close()
	stopDaemon()
	runDaemon(t, root)
	if moved, _ := url.Parse(signinLink(t, root)); moved.Host == dl.Host {
		t.Fatalf("the daemon came back on its old address %s", moved.Host)
	}
	killMaster(t, h, env.RunDir)
	eventually(t, "the second reconnect", 60*time.Second, func() bool {
		return strings.Count(r.said(), "connected to test again") == 2
	})
	eventually(t, "the dashboard on its new port, through the same local port", 20*time.Second, func() bool {
		code, body := dashGet(t, base, session, "/api/threads")
		return code == http.StatusOK && strings.Contains(body, "reply over ssh")
	})

	if code := r.stop(t); code != 0 {
		t.Fatalf("stopped: exit %d:\n%s", code, r.said())
	}
	for _, tok := range []string{sessionTokenOf(t, link), sessionTokenOf(t, fresh)} {
		if strings.Contains(r.said(), tok) || strings.Contains(second.said(), tok) {
			t.Fatal("connect wrote a link to its log")
		}
	}
	noSockets(t, env.RunDir)
	if _, err := http.Get(base + "/"); err == nil {
		t.Fatal("the local port still answers after connect stopped")
	}
}

// readAllEvents is the position of every event the daemon has.
func readAllEvents(t *testing.T, c *wire.Client) []uint64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Events(ctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []uint64
	for e := range ch {
		if e.Kind == control.CaughtUp {
			return out
		}
		out = append(out, e.Pos)
	}
	return out
}

// readUntilCaughtUp is the positions a dashboard stream sends up to its
// caught_up mark.
func readUntilCaughtUp(t *testing.T, events <-chan sseEvent) []uint64 {
	t.Helper()
	var out []uint64
	for {
		select {
		case m, ok := <-events:
			if !ok {
				t.Fatal("the stream ended before it caught up")
			}
			if m.kind == "caught_up" {
				return out
			}
			out = append(out, m.id)
		case <-time.After(20 * time.Second):
			t.Fatalf("the stream did not catch up after %v", out)
		}
	}
}

// killMaster kills the shared connection's ssh, as a dropped network would
// end it.
func killMaster(t *testing.T, h *sshHost, runDir string) {
	t.Helper()
	cms, _ := filepath.Glob(filepath.Join(runDir, "*.cm"))
	if len(cms) != 1 {
		t.Fatalf("control sockets: %v", cms)
	}
	out, _ := exec.Command(h.ssh, "-S", cms[0], "-O", "check", "test").CombinedOutput()
	m := regexp.MustCompile(`pid=(\d+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("no master: %s", out)
	}
	pid, _ := strconv.Atoi(string(m[1]))
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

// What it remembers: no argument is the last host; a remembered local port
// that is taken is replaced by a free one, which is remembered.
func TestConnectRemembersTheHost(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	h := startSSHD(t)
	l := newLauncher(t)
	env := connectEnv{SSH: h.ssh, Open: l.path, StateFile: filepath.Join(shortDir(t), "connect.json"), RunDir: filepath.Join(shortDir(t), "run")}
	if code, out := runConnect(t, env); code != exitError || !strings.Contains(out, "no host to connect to") {
		t.Fatalf("with nothing remembered: exit %d, %q", code, out)
	}
	// Another host remembers a port, which this one may not take, even
	// when it remembers it too: two dashboards never share a local origin.
	shared, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sharedPort := shared.Addr().(*net.TCPAddr).Port
	shared.Close()
	os.WriteFile(env.StateFile, []byte(fmt.Sprintf(`{"hosts": {"other": {"port": %d}, "test": {"port": %d}}}`, sharedPort, sharedPort)), 0o600)
	r := startConnect(t, env, "test", "--root", root, "--hataori", os.Args[0])
	first, _ := url.Parse(l.opened(t, 1))
	_, port, _ := net.SplitHostPort(first.Host)
	if port == strconv.Itoa(sharedPort) {
		t.Fatalf("it took port %d, which another host remembers", sharedPort)
	}
	if code := r.stop(t); code != 0 {
		t.Fatalf("exit %d:\n%s", code, r.said())
	}

	// The remembered port is taken now.
	taken, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	r = startConnect(t, env) // no argument: the last host, its root and its hataori
	second, _ := url.Parse(l.opened(t, 2))
	_, port2, _ := net.SplitHostPort(second.Host)
	if port2 == port {
		t.Fatalf("the taken port %s was used", port)
	}
	openLink(t, second.String())
	if code := r.stop(t); code != 0 {
		t.Fatalf("exit %d:\n%s", code, r.said())
	}
	b, _ := os.ReadFile(env.StateFile)
	if !strings.Contains(string(b), `"port": `+port2) || !strings.Contains(string(b), `"other"`) {
		t.Fatalf("the new port is not remembered, or the other host was lost: %s", b)
	}
	// Asked for a port that is taken, it says so.
	if code, out := runConnect(t, env, "--port", port); code != exitError || !strings.Contains(out, "is taken") {
		t.Fatalf("--port taken: exit %d, %q", code, out)
	}
}

// runConnect runs `hataori connect` until it ends by itself and returns its
// exit code and what it said.
func runConnect(t *testing.T, env connectEnv, args ...string) (int, string) {
	t.Helper()
	r := startConnect(t, env, args...)
	return r.wait(t), r.said()
}

// Each failure stops it with a message that names the cause: no ssh, no
// hataori on the host, forwarding refused by the server, and a link that
// does not name a loopback address. Nothing is left in the run directory.
func TestConnectFailures(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	l := newLauncher(t)
	h := startSSHD(t)
	refusing := startSSHD(t, "AllowTcpForwarding no")
	// A hataori on the host whose signin prints a link of another listener:
	// an old one printed the web address.
	printing := func(link string) string {
		p := filepath.Join(shortDir(t), "hataori")
		os.WriteFile(p, []byte("#!/bin/sh\necho '{\"link\":\""+link+"\"}'\n"), 0o700)
		return p
	}
	for _, c := range []struct {
		name string
		ssh  string
		args []string
		want string
	}{
		{"no ssh", "", []string{"test"}, "no ssh"},
		{"no hataori on the host", h.ssh, []string{"test", "--root", root, "--hataori", "/nonexistent/hataori"}, "--hataori"},
		{"forwarding refused", refusing.ssh, []string{"test", "--root", root, "--hataori", os.Args[0]}, "refused to forward"},
		{"a web link", h.ssh, []string{"test", "--hataori", printing("https://dash.example.com/signin?t=x")}, "does not name a loopback address"},
		{"https on loopback", h.ssh, []string{"test", "--hataori", printing("https://127.0.0.1:7466/signin#t=x")}, "does not name a loopback address"},
		{"the token in the query", h.ssh, []string{"test", "--hataori", printing("http://127.0.0.1:7466/signin?t=x")}, "does not name a loopback address"},
		{"another path", h.ssh, []string{"test", "--hataori", printing("http://127.0.0.1:7466/x#t=x")}, "does not name a loopback address"},
		{"http elsewhere", h.ssh, []string{"test", "--hataori", printing("http://192.0.2.1:7466/signin#t=x")}, "does not name a loopback address"},
		{"a quote in the root", h.ssh, []string{"test", "--root", "/srv/it's"}, "has a quote"},
		{"not a host", h.ssh, []string{"-oProxyCommand=x"}, "flag provided but not defined"},
		{"a host that looks like an option", h.ssh, []string{"--", "-oProxyCommand=x"}, "not a host to connect to"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := connectEnv{SSH: c.ssh, Open: l.path, StateFile: filepath.Join(shortDir(t), "connect.json"), RunDir: filepath.Join(shortDir(t), "run")}
			code, out := runConnect(t, env, c.args...)
			if code == 0 || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, %q; want a message with %q", code, out, c.want)
			}
			noSockets(t, env.RunDir)
		})
	}
	if b, _ := os.ReadFile(l.record); len(b) != 0 {
		t.Fatalf("a failed connect opened %q", b)
	}
}

// The run directory must be the person's alone.
func TestConnectRefusesAnOpenRunDirectory(t *testing.T) {
	d := filepath.Join(shortDir(t), "run")
	os.Mkdir(d, 0o755)
	os.Chmod(d, 0o755)
	env := connectEnv{SSH: "/bin/true", StateFile: filepath.Join(shortDir(t), "c.json"), RunDir: d}
	if code, out := runConnect(t, env, "test"); code != exitError || !strings.Contains(out, "only you can open") {
		t.Fatalf("exit %d, %q", code, out)
	}
}

// checkCalls checks every ssh call connect made: the control socket, the
// forward and the keep-alive only, and never the link.
func checkCalls(t *testing.T, h *sshHost, runDir, root, dashboard, token string) {
	t.Helper()
	ctl := regexp.MustCompile(`^` + regexp.QuoteMeta(runDir) + `/[0-9a-f]{8}\.cm$`)
	fwd := regexp.MustCompile(`^` + regexp.QuoteMeta(runDir) + `/[0-9a-f]{8}\.fwd:` + regexp.QuoteMeta(dashboard) + `$`)
	signin := fmt.Sprintf("%s signin --root %s", quoteSh(os.Args[0]), quoteSh(root))
	forwards := 0
	for _, c := range h.calls(t) {
		if strings.Contains(strings.Join(c, " "), token) {
			t.Fatalf("an ssh call carries the link: %q", c)
		}
		if c[0] == "-M" {
			if len(c) != 12 || !ctl.MatchString(c[2]) || !slices.Equal(c[3:], []string{"-o", "ControlPersist=no", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-N", "--", "test"}) {
				t.Fatalf("the master: %q", c)
			}
			continue
		}
		if len(c) < 5 || c[0] != "-S" || !ctl.MatchString(c[1]) || c[2] != "-o" || c[3] != "ControlMaster=no" {
			t.Fatalf("a call that does not go through the control socket alone: %q", c)
		}
		switch rest := c[4:]; {
		case slices.Equal(rest, []string{"-O", "check", "--", "test"}), slices.Equal(rest, []string{"-O", "exit", "--", "test"}):
		case slices.Equal(rest, []string{"--", "test", signin}):
		case len(rest) == 6 && rest[0] == "-O" && rest[1] == "forward" && rest[2] == "-L" && fwd.MatchString(rest[3]) && rest[4] == "--" && rest[5] == "test":
			forwards++
		default:
			t.Fatalf("an ssh call with more than the control socket, the forward and the keep-alive: %q", c)
		}
	}
	if forwards == 0 {
		t.Fatal("no forward was asked for")
	}
}

// noSockets fails the test when the run directory holds a socket.
func noSockets(t *testing.T, runDir string) {
	t.Helper()
	entries, _ := os.ReadDir(runDir)
	for _, e := range entries {
		if e.Type()&os.ModeSocket != 0 {
			t.Fatalf("left in the run directory: %s", e.Name())
		}
	}
}

// The test's ssh records every call whole, also calls made at the same
// moment, which TestConnectTwoAtOnce counts (#478).
func TestSSHWrapperRecordsCallsWhole(t *testing.T) {
	h := startSSHD(t)
	args := []string{"-V", "-M", "-S", "/run/x.cm", "-o", "ControlPersist=no", "-o", "ServerAliveInterval=15", "-N", "--", "test"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				exec.Command(h.ssh, args...).Run() // -V: ssh prints its version and ends
			}
		}()
	}
	wg.Wait()
	calls := h.calls(t)
	if len(calls) != 200 {
		t.Fatalf("%d calls recorded, want 200", len(calls))
	}
	for _, c := range calls {
		if !slices.Equal(c, args) {
			t.Fatalf("a call recorded as %q", c)
		}
	}
}

// Two connects for one host started at once: one connects, and the other
// has it open a link and ends; there is one shared connection.
func TestConnectTwoAtOnce(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	h := startSSHD(t)
	l := newLauncher(t)
	env := connectEnv{SSH: h.ssh, Open: l.path, StateFile: filepath.Join(shortDir(t), "connect.json"), RunDir: filepath.Join(shortDir(t), "run")}
	args := []string{"test", "--root", root, "--hataori", os.Args[0]}
	a, b := startConnect(t, env, args...), startConnect(t, env, args...)
	var ended, running *connectRun
	eventually(t, "one of the two to end", 60*time.Second, func() bool {
		for _, x := range [][2]*connectRun{{a, b}, {b, a}} {
			select {
			case <-x[0].finished:
				ended, running = x[0], x[1]
				return true
			default:
			}
		}
		return false
	})
	if ended.code != 0 {
		t.Fatalf("the second: exit %d:\n%s", ended.code, ended.said())
	}
	l.opened(t, 2)
	masters := 0
	for _, c := range h.calls(t) {
		if c[0] == "-M" {
			masters++
		}
	}
	if masters != 1 {
		t.Fatalf("%d shared connections", masters)
	}
	if code := running.stop(t); code != 0 {
		t.Fatalf("the first: exit %d:\n%s", code, running.said())
	}
}

// A login script on the host that prints a line before hataori's does not
// stop it: the link is the last line.
func TestConnectReadsTheLastLine(t *testing.T) {
	root := newRoot(t)
	withDashboard(t, root, map[string]any{"listen": "127.0.0.1:0"})
	startDaemon(t, root)
	h := startSSHD(t)
	l := newLauncher(t)
	chatty := filepath.Join(shortDir(t), "hataori")
	os.WriteFile(chatty, []byte("#!/bin/sh\necho 'Welcome to the host.'\nexec "+quoteSh(os.Args[0])+" \"$@\"\n"), 0o700)
	env := connectEnv{SSH: h.ssh, Open: l.path, StateFile: filepath.Join(shortDir(t), "connect.json"), RunDir: filepath.Join(shortDir(t), "run")}
	r := startConnect(t, env, "test", "--root", root, "--hataori", chatty)
	openLink(t, l.opened(t, 1))
	if code := r.stop(t); code != 0 {
		t.Fatalf("exit %d:\n%s", code, r.said())
	}
}
