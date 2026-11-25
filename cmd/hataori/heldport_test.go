package main

import (
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
)

// portHolder keeps a loopback port for a test: it listens on it for as long
// as the test runs, and hands the connections to the daemon's listener
// while there is one. Nobody else can take the port before the daemon
// listens, nor while it is restarted.
type portHolder struct {
	ln net.Listener
	mu sync.Mutex
	to *heldListener // the daemon's, nil while it does not listen
}

// heldPorts are the ports held now, by address.
var heldPorts sync.Map // string: *portHolder

func init() {
	dashboardBind = func(network, address string) (net.Listener, error) {
		if h, ok := heldPorts.Load(address); ok {
			return h.(*portHolder).listen()
		}
		return net.Listen(network, address)
	}
}

// heldPort returns a loopback address that only a dashboard of this
// process can listen on until the test ends, and again after a restart.
// While none does, a connection to it is closed at once.
func heldPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, address := &portHolder{ln: ln}, ln.Addr().String()
	heldPorts.Store(address, h)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.run()
	}()
	t.Cleanup(func() {
		heldPorts.Delete(address)
		ln.Close()
		<-done
	})
	return address
}

func (h *portHolder) run() {
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		l := h.to
		h.mu.Unlock()
		if l == nil {
			c.Close()
			continue
		}
		select {
		case l.conns <- c:
		case <-l.closed:
			c.Close()
		}
	}
}

func (h *portHolder) listen() (net.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.to != nil {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: h.ln.Addr(), Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	h.to = &heldListener{h: h, conns: make(chan net.Conn), closed: make(chan struct{})}
	return h.to, nil
}

// heldListener is a daemon's listener on a held port.
type heldListener struct {
	h      *portHolder
	conns  chan net.Conn
	closed chan struct{}
}

func (l *heldListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case c := <-l.conns:
		return c, nil
	}
}

func (l *heldListener) Close() error {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if l.h.to != l {
		return net.ErrClosed
	}
	l.h.to = nil
	close(l.closed)
	return nil
}

func (l *heldListener) Addr() net.Addr { return l.h.ln.Addr() }

// The rig's ports cannot be taken before its daemon listens, nor while the
// daemon is restarted (#696): whoever tries gets none, and the daemon comes
// up.
func TestHeldPortsAreNotTaken(t *testing.T) {
	d := newPasskeyDash(t)
	for range 2 {
		var taken []net.Listener
		for _, a := range []string{d.local, d.listen} {
			if ln, err := net.Listen("tcp", a); err == nil {
				taken = append(taken, ln)
			}
		}
		stop := runDaemon(t, d.root)
		if link := signinLink(t, d.root); openLink(t, link) == "" {
			t.Fatal("no session on the loopback listener")
		}
		if code, _ := d.do("GET", "", "/", "", "", nil); code != 200 {
			t.Fatalf("the web listener's page: %d", code)
		}
		stop()
		for _, ln := range taken {
			ln.Close()
		}
	}
}
