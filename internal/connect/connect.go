// Package connect is `hataori connect`: the dashboard of an instance on
// another machine, reached over SSH (design, section 7.2, "Over SSH
// without a chore"). It runs on the person's machine, Linux or macOS.
//
// It runs the system's ssh, so the person's keys, agent and config apply,
// and it writes no SSH key and no SSH config. It opens one shared
// connection (OpenSSH's ControlMaster, its control socket in a directory
// only the person can open) and runs `hataori signin` on the host through
// it. It listens on a local port, bound to 127.0.0.1, for the whole run,
// and passes each connection on through the shared connection to the
// loopback address the link names (ssh forwards a Unix socket in that
// directory there). It opens the browser at the link with the local port.
// It keeps running: when the connection drops it connects again, and since
// it never lets go of the port, nobody else can take it in between, and an
// open page goes on by itself.
//
// The link is kept in memory only: it is written to no file and no log,
// and passed to no process but the browser's launcher.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options is one run of connect.
type Options struct {
	// Target is [user@]host as the person gave it; empty for the host
	// connected to last.
	Target string
	// Root is the instance root on the host (--root); empty for the one
	// remembered for the host, else none, and the host's $HATAORI_ROOT.
	Root string
	// Hataori is the hataori binary on the host (--hataori), for a
	// non-interactive SSH session whose PATH does not have it; empty for
	// the one remembered, else "hataori".
	Hataori string
	// Port is the local port (--port); 0 for the one remembered for the
	// host if it is free, else the dashboard's own number if that is free,
	// else any free port; never one remembered for another host.
	Port int

	// SSH is the ssh command; Open the browser's launcher, given the link.
	SSH, Open string
	// StateFile is where hosts are remembered; RunDir a directory only the
	// person can open, for the control sockets.
	StateFile, RunDir string
	// Log is where the person reads what happens. The link never goes there.
	Log io.Writer
	// Backoff is how long to wait before connecting again after the attempt
	// that failed; nil for 1 s doubling to 30 s.
	Backoff func(attempt int) time.Duration
	// Probe is how long the dashboard has to answer through the forward;
	// 10 s when zero.
	Probe time.Duration
}

// Error is a reason connect stops, in words for the person.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func fail(format string, a ...any) error { return &Error{fmt.Sprintf(format, a...)} }

// state is what is remembered, per host, in StateFile.
type state struct {
	Last  string          `json:"last,omitempty"`
	Hosts map[string]host `json:"hosts,omitempty"`
}

type host struct {
	Root    string `json:"root,omitempty"`
	Hataori string `json:"hataori,omitempty"`
	Port    int    `json:"port,omitempty"`
}

func readState(path string) (state, error) {
	var st state
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// remember records h for target as the last host. It reads the file again
// first, so a connect for another host that wrote it meanwhile keeps its
// entry, and replaces the file whole, readable by the person only.
func remember(path, target string, h host) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil { // a connect for another host may write it too
		return err
	}
	st, err := readState(path)
	if err != nil {
		return err
	}
	st.Last = target
	if st.Hosts == nil {
		st.Hosts = map[string]host{}
	}
	st.Hosts[target] = h
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".connect-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; left only when a step failed
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// validTarget reports whether t can be given to ssh as its destination:
// [user@]host, no spaces, and nothing ssh would read as an option.
func validTarget(t string) bool {
	return t != "" && !strings.HasPrefix(t, "-") && !strings.ContainsAny(t, " \t\r\n'\"\\;|&$`<>()")
}

// validRemote reports whether s can go in the remote command, quoted: no
// quote, backslash or line end, which shells other than a POSIX one would
// read differently inside single quotes.
func validRemote(s string) bool { return !strings.ContainsAny(s, "'\\\r\n\x00") }

// key names the files of one host in RunDir: short, since a Unix socket's
// path is short, and ssh adds to the control socket's while it makes it.
func key(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:4])
}

// maxSocket is the longest Unix socket path both Linux and macOS take,
// less what ssh adds to a control socket's path while it makes it.
const maxSocket = 103 - 17

// PrivateDir makes dir, a directory only this user can open, or checks
// that it is one: owned by this user, not a link, with no access for
// anyone else.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 || !ownedByMe(fi) {
		return fail("%s is not a directory only you can open: remove it, or fix its owner and mode (0700)", dir)
	}
	return nil
}

// Run connects as opts say until ctx ends. When a connect for the same
// host already runs, it asks that one for a fresh link and returns.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Backoff == nil {
		opts.Backoff = func(n int) time.Duration {
			d := time.Second << min(n, 5)
			return min(d, 30*time.Second)
		}
	}
	if opts.Probe <= 0 {
		opts.Probe = 10 * time.Second
	}
	st, err := readState(opts.StateFile)
	if err != nil {
		return fail("the remembered hosts cannot be read: %v", err)
	}
	asked := opts.Root != "" || opts.Hataori != "" || opts.Port != 0
	if opts.Target == "" {
		if st.Last == "" {
			return fail("no host to connect to: give one, as [user@]host")
		}
		opts.Target = st.Last
	}
	if !validTarget(opts.Target) {
		return fail("%q is not a host to connect to, such as user@host", opts.Target)
	}
	h := st.Hosts[opts.Target]
	if opts.Root == "" {
		opts.Root = h.Root
	}
	if opts.Hataori == "" {
		opts.Hataori = h.Hataori
	}
	for name, v := range map[string]string{"--root": opts.Root, "--hataori": opts.Hataori} {
		if !validRemote(v) {
			return fail("%s %q has a quote, a backslash or a line end, which the host's shell may read differently", name, v)
		}
	}
	if opts.SSH == "" {
		return fail("no ssh: hataori connect runs the system's OpenSSH client, and none is on PATH")
	}
	if err := PrivateDir(opts.RunDir); err != nil {
		return err
	}
	base := filepath.Join(opts.RunDir, key(opts.Target))
	c := &conn{opts: opts, ctl: base + ".cm", fwd: base + ".fwd", ipc: base + ".sock"}
	if len(c.ctl) > maxSocket || len(c.fwd) > maxSocket {
		return fail("%s is too long a path for the sockets hataori connect needs", opts.RunDir)
	}

	// One connect per host, by a lock held for the run: a second one asks
	// the first, over its socket, for a fresh link.
	lock, err := lockFile(base + ".lock")
	if err != nil {
		return err
	}
	if lock == nil {
		if asked {
			fmt.Fprintf(opts.Log, "hataori connect: a connect runs for %s already, and keeps its own --root, --hataori and --port.\n", opts.Target)
		}
		return c.askRunning(ctx)
	}
	defer lock.Close()
	c.leftOver()
	ipc, err := net.Listen("unix", c.ipc)
	if err != nil {
		return fail("cannot listen at %s: %v", c.ipc, err)
	}
	defer ipc.Close() // also removes the socket
	// A second connect is answered once this one is up; until then its
	// request waits, as long as this one waits for the person.
	ready := make(chan struct{})
	go func() {
		for {
			cn, err := ipc.Accept()
			if err != nil {
				return
			}
			go func() {
				select {
				case <-ready:
					c.serveRequest(ctx, cn)
				case <-ctx.Done():
					cn.Close()
				}
			}()
		}
	}()

	if err := c.connect(ctx); err != nil {
		c.stop()
		return err
	}
	defer c.stop()
	l, err := c.signin(ctx)
	if err != nil {
		return err
	}
	others := map[int]bool{}
	for t, o := range st.Hosts {
		if t != opts.Target && o.Port > 0 {
			others[o.Port] = true
		}
	}
	ln, err := listenPort(opts.Port, h.Port, l.port, others)
	if err != nil {
		return err
	}
	defer ln.Close()
	c.local = ln.Addr().(*net.TCPAddr).Port
	go c.serve(ln)
	if err := c.forward(ctx, l); err != nil {
		return err
	}
	if err := remember(opts.StateFile, opts.Target, host{Root: opts.Root, Hataori: opts.Hataori, Port: c.local}); err != nil {
		fmt.Fprintf(opts.Log, "hataori connect: the host is not remembered: %v\n", err)
	}
	if err := c.open(l); err != nil {
		return err
	}
	fmt.Fprintf(opts.Log, "hataori connect: the dashboard of %s is at http://127.0.0.1:%d; this keeps the connection up until it is stopped (Ctrl-C).\n", opts.Target, c.local)
	close(ready)
	for attempt := 0; ; {
		select {
		case <-ctx.Done():
			return nil
		case <-c.masterDone:
			fmt.Fprintf(opts.Log, "hataori connect: the connection to %s dropped; connecting again.\n", opts.Target)
			for {
				d := opts.Backoff(attempt)
				attempt++
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(d):
				}
				err := c.reconnect(ctx)
				if err == nil {
					fmt.Fprintf(opts.Log, "hataori connect: connected to %s again; the dashboard is at http://127.0.0.1:%d.\n", opts.Target, c.local)
					attempt = 0
					break
				}
				if ctx.Err() != nil {
					return nil
				}
				c.stop()
				fmt.Fprintf(opts.Log, "hataori connect: %v; trying again.\n", err)
			}
		}
	}
}

// lockFile takes the lock of path, a file only this user can open, and
// returns it held, or nil when another process holds it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fail("cannot open %s: %v", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fail("cannot lock %s: %v", path, err)
	}
	return f, nil
}

// conn is the shared connection to one host.
type conn struct {
	opts          Options
	ctl, fwd, ipc string
	local         int // the local port

	mu         sync.Mutex
	remote     string // the dashboard's loopback address on the host
	master     *exec.Cmd
	masterDone chan struct{}
	prohibited bool // the master said the server refused a forward
	signinMu   sync.Mutex
}

// follow is the arguments of a call through the shared connection: the
// control socket, and never a master of its own, whatever the person's
// SSH config says.
func (c *conn) follow(args ...string) []string {
	return append([]string{"-S", c.ctl, "-o", "ControlMaster=no"}, args...)
}

// ssh runs ssh with args and returns its standard output and error.
func (c *conn) ssh(ctx context.Context, args ...string) ([]byte, []byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, c.opts.SSH, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// leftOver ends what a connect for this host that was killed left: its
// master, through its control socket, and its sockets.
func (c *conn) leftOver() {
	if _, err := os.Lstat(c.ctl); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c.ssh(ctx, c.follow("-O", "exit", "--", c.opts.Target)...)
		cancel()
	}
	for _, p := range []string{c.ctl, c.fwd, c.ipc} {
		os.Remove(p) // sockets of this host in the person's own run directory, under its lock
	}
}

// watch passes what ssh writes on to the person as it comes, prompts
// without a line end included, and notes a forward the server refused.
type watch struct {
	c    *conn
	tail []byte
}

func (w *watch) Write(p []byte) (int, error) {
	w.c.opts.Log.Write(p)
	w.tail = append(w.tail, p...)
	if bytes.Contains(w.tail, []byte("administratively prohibited")) {
		w.c.mu.Lock()
		w.c.prohibited = true
		w.c.mu.Unlock()
		w.tail = nil
	}
	if len(w.tail) > 64 {
		w.tail = w.tail[len(w.tail)-64:]
	}
	return len(p), nil
}

// connect starts the shared connection and waits until it is ready. ssh
// asks the person for whatever it needs on the terminal, as it would.
func (c *conn) connect(ctx context.Context) error {
	os.Remove(c.ctl) // a master that was killed leaves its socket; this connect holds the host's lock
	cmd := exec.Command(c.opts.SSH, "-M", "-S", c.ctl, "-o", "ControlPersist=no",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-N", "--", c.opts.Target)
	cmd.Stderr = &watch{c: c}
	if err := cmd.Start(); err != nil {
		return fail("ssh cannot be started: %v", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	c.mu.Lock()
	c.master, c.masterDone = cmd, done
	c.mu.Unlock()
	for {
		if _, _, err := c.ssh(ctx, c.follow("-O", "check", "--", c.opts.Target)...); err == nil {
			return nil
		}
		select {
		case <-done:
			return fail("ssh to %s ended before the connection was up: see what it said above", c.opts.Target)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// reconnect starts the shared connection again, asks the host for the
// dashboard's address again (it may have come back on another port), and
// forwards to it.
func (c *conn) reconnect(ctx context.Context) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	l, err := c.signin(ctx) // the link is used for its address only, and dropped
	if err != nil {
		return err
	}
	return c.forward(ctx, l)
}

// stop ends the shared connection.
func (c *conn) stop() {
	c.mu.Lock()
	master, done := c.master, c.masterDone
	c.mu.Unlock()
	if master == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.ssh(ctx, c.follow("-O", "exit", "--", c.opts.Target)...)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		master.Process.Kill()
		<-done
	}
	os.Remove(c.ctl) // the control socket of this run, if ssh left it
	os.Remove(c.fwd) // the forward's socket of this run, if ssh left it
}

// link is a sign-in link of the host's loopback listener.
type link struct {
	u    *url.URL
	host string // the loopback address it names
	port int
}

// quote is s for a POSIX shell, in single quotes.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// signin runs `hataori signin` on the host through the shared connection
// and returns the link, which goes nowhere but memory.
func (c *conn) signin(ctx context.Context) (link, error) {
	c.signinMu.Lock()
	defer c.signinMu.Unlock()
	bin := c.opts.Hataori
	if bin == "" {
		bin = "hataori"
	}
	cmd := []string{quote(bin), "signin"}
	if c.opts.Root != "" {
		cmd = append(cmd, "--root", quote(c.opts.Root))
	}
	out, errOut, err := c.ssh(ctx, c.follow("--", c.opts.Target, strings.Join(cmd, " "))...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 127 {
			return link{}, fail("%s is not found on %s: give its path there with --hataori", bin, c.opts.Target)
		}
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = err.Error()
		}
		return link{}, fail("hataori signin on %s failed: %s", c.opts.Target, msg)
	}
	// The last line: a login script of the host may print before it.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var v struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &v); err != nil || v.Link == "" {
		return link{}, fail("hataori signin on %s printed no link: is it hataori?", c.opts.Target)
	}
	return parseLink(v.Link)
}

// parseLink checks that raw is a link of a loopback listener: http, on
// localhost, 127.0.0.1 or [::1], with a port.
func parseLink(raw string) (link, error) {
	u, err := url.Parse(raw)
	bad := fail("the link hataori signin printed does not name a loopback address: the instance needs a loopback listener (\"dashboard.listen\"), and hataori on the host may be too old")
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "/signin" || u.RawQuery != "" || !strings.HasPrefix(u.Fragment, "t=") {
		return link{}, bad
	}
	h, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		return link{}, bad
	}
	if h != "localhost" {
		if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
			return link{}, bad
		}
	}
	port, err := strconv.Atoi(p)
	if err != nil || port <= 0 || port > 65535 {
		return link{}, bad
	}
	return link{u: u, host: h, port: port}, nil
}

// listenPort listens on the local port: the one asked for; else the
// remembered one, else the dashboard's own number, if free and no other
// host's; else any free port.
func listenPort(asked, remembered, remote int, others map[int]bool) (net.Listener, error) {
	at := func(p int) (net.Listener, error) {
		return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	}
	if asked > 0 {
		ln, err := at(asked)
		if err != nil {
			return nil, fail("the local port %d is taken: give another with --port, or none", asked)
		}
		return ln, nil
	}
	for _, p := range []int{remembered, remote} {
		if p > 0 && !others[p] {
			if ln, err := at(p); err == nil {
				return ln, nil
			}
		}
	}
	for range 20 {
		ln, err := at(0)
		if err != nil || !others[ln.Addr().(*net.TCPAddr).Port] {
			return ln, err
		}
		defer ln.Close() // another host's: held until a port of its own comes
	}
	return nil, fail("no free local port that no other host remembers")
}

// serve passes each connection to the local port on through the forward.
// While the shared connection is down, a connection is closed at once.
func (c *conn) serve(ln net.Listener) {
	for {
		cn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer cn.Close()
			up, err := net.Dial("unix", c.fwd)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, cn); done <- struct{}{} }()
			go func() { io.Copy(cn, up); done <- struct{}{} }()
			<-done
		}()
	}
}

// forward has ssh forward a Unix socket in the run directory to the
// dashboard's address l names, through the shared connection, and checks
// that the dashboard answers through the local port.
func (c *conn) forward(ctx context.Context, l link) error {
	remote := net.JoinHostPort(l.host, strconv.Itoa(l.port))
	os.Remove(c.fwd) // the forward's socket of a master that ended; this connect holds the host's lock
	spec := c.fwd + ":" + remote
	if _, errOut, err := c.ssh(ctx, c.follow("-O", "forward", "-L", spec, "--", c.opts.Target)...); err != nil {
		return fail("the dashboard cannot be forwarded: %s", strings.TrimSpace(string(errOut)))
	}
	c.mu.Lock()
	c.remote, c.prohibited = remote, false
	c.mu.Unlock()
	hc := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(c.opts.Probe)
	for {
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", c.local), nil)
		resp, err := hc.Do(r)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s", resp.Status)
		}
		c.mu.Lock()
		prohibited := c.prohibited
		c.mu.Unlock()
		if prohibited {
			return fail("%s refused to forward the port: its sshd does not allow it (AllowTcpForwarding)", c.opts.Target)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fail("the dashboard does not answer through the forward (%v): %s may refuse to forward it (AllowTcpForwarding), or the dashboard does not listen at %s", err, c.opts.Target, remote)
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// open opens the browser at l with the local port, on 127.0.0.1: the
// address connect listens on, which a name such as localhost might not
// resolve to. It does not wait for the launcher, which may run the browser
// in the foreground.
func (c *conn) open(l link) error {
	u := *l.u
	u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(c.local))
	if c.opts.Open == "" {
		fmt.Fprintf(c.opts.Log, "hataori connect: no browser launcher to open the link with; run hataori connect again on a machine with one\n")
		return nil
	}
	cmd := exec.Command(c.opts.Open, u.String())
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return fail("the browser could not be opened with %s: %v", filepath.Base(c.opts.Open), err)
	}
	go cmd.Wait()
	return nil
}

// The requests a second connect for the same host sends the running one.
const (
	reqOpen = "open"
	repOK   = "ok"
)

// askRunning asks the connect that runs for the host, which holds its
// lock, to open a fresh link. That one may still be starting: its socket
// is waited for.
func (c *conn) askRunning(ctx context.Context) error {
	var cn net.Conn
	deadline := time.Now().Add(2 * time.Minute) // the other one may wait for a passphrase
	for {
		var d net.Dialer
		var err error
		if cn, err = d.DialContext(ctx, "unix", c.ipc); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fail("a hataori connect runs for %s, but does not answer: stop it, or wait until it is connected", c.opts.Target)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer cn.Close()
	cn.SetDeadline(time.Now().Add(10 * time.Minute)) // the other one may still wait for a passphrase
	fmt.Fprintln(cn, reqOpen)
	reply, err := bufio.NewReader(cn).ReadString('\n')
	if err != nil {
		return fail("the hataori connect that runs for %s did not answer: %v", c.opts.Target, err)
	}
	if reply = strings.TrimSpace(reply); reply != repOK {
		return fail("the hataori connect that runs for %s: %s", c.opts.Target, reply)
	}
	fmt.Fprintf(c.opts.Log, "hataori connect: the connect that runs for %s opened a fresh sign-in link.\n", c.opts.Target)
	return nil
}

// serveRequest answers one request of a second connect. A request that
// does not come in a few seconds is dropped.
func (c *conn) serveRequest(ctx context.Context, cn net.Conn) {
	defer cn.Close()
	cn.SetReadDeadline(time.Now().Add(5 * time.Second))
	req, err := bufio.NewReader(cn).ReadString('\n')
	if err != nil || strings.TrimSpace(req) != reqOpen {
		return
	}
	cn.SetDeadline(time.Now().Add(time.Minute))
	// Only through the shared connection: without it ssh would open one of
	// its own, and might ask the person for something where nobody sees it.
	if _, _, err := c.ssh(ctx, c.follow("-O", "check", "--", c.opts.Target)...); err != nil {
		fmt.Fprintf(cn, "the connection to %s is down and being made again: try again in a moment\n", c.opts.Target)
		return
	}
	l, err := c.signin(ctx)
	if err == nil {
		err = c.open(l)
	}
	if err != nil {
		fmt.Fprintf(cn, "%s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}
	fmt.Fprintln(cn, repOK)
}

// ownedByMe reports whether fi belongs to the user this runs as.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
