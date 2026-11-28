package nativehttp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	// DiscoveryFile is private instance state, not a public temporary pointer.
	DiscoveryFile = "native-http.json"
	leaseFile     = "native-http.lock"
	// Darwin's sockaddr_un has 104 bytes including the terminating NUL.
	maxSocketPath = 103
)

var ErrBusy = errors.New("native HTTP: another listener holds the instance lease")

// Config is supplied by the daemon after it acquires the instance lock.
// Instance is the installation identity, Version the bundled daemon version.
// Neither value may come from a request. RuntimeDir overrides the short
// runtime parent for isolated tests; empty uses the system's /tmp directory.
type Config struct {
	Root       string
	Instance   string
	Version    string
	RuntimeDir string
}

// Listener admits only kernel-confirmed peers of the daemon's OS user.
// Its HTTP consumer must separately enforce routes, methods, Host/Origin and
// assign an owner Caller only on this listener. It never opens a TCP socket.
type Listener struct {
	ln       *net.UnixListener
	root     *os.Root
	state    *os.Root
	lease    *os.File
	path     string
	socket   fs.FileInfo
	once     sync.Once
	closeErr error
}

// Listen opens a fresh socket and publishes its binding only after its mode
// is checked. The daemon's state/lock is never opened, removed or unlocked.
// A separate permanent lease file prevents duplicate native listeners. Old
// socket names are never reused or removed on startup, even after a crash.
func Listen(cfg Config) (*Listener, error) {
	return listen(cfg, func(path string) error { return os.Chmod(path, 0o600) })
}

// The mode setter is local to one construction; tests exercise an actual
// failure after bind without changing a process-wide hook or permissions.
func listen(cfg Config, setMode func(string) error) (_ *Listener, err error) {
	if err := validConfig(cfg); err != nil {
		return nil, err
	}
	r, err := openPrivateRoot(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("native HTTP: root: %w", err)
	}
	l := &Listener{root: r}
	defer func() {
		if err != nil {
			l.Close()
		}
	}()
	l.state, err = privateDir(r, "state", true)
	if err != nil {
		return nil, fmt.Errorf("native HTTP: state: %w", err)
	}
	l.lease, err = privateFile(l.state, leaseFile, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, fmt.Errorf("native HTTP: lease: %w", err)
	}
	if err := syscall.Flock(int(l.lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("native HTTP: lease: %w", err)
	}
	// Existing discovery must be a private ordinary file bound to this root
	// and installation. A changed daemon version may replace it under lease.
	if _, err := readDiscovery(l.state, r, cfg.Instance); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	generation := make([]byte, 12)
	if _, err := rand.Read(generation); err != nil {
		return nil, err
	}
	name := hex.EncodeToString(generation)
	l.path, err = socketPath(r, cfg, name, true)
	if err != nil {
		return nil, err
	}
	l.ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: l.path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("native HTTP: listen: %w", err)
	}
	// Capture identity before turning off the listener's initial cleanup.
	// No untrusted user can reach this pathname through the private parent.
	l.socket, err = os.Lstat(l.path)
	if err != nil {
		return nil, err
	}
	l.ln.SetUnlinkOnClose(false)
	// The parent is already 0700, including throughout socket creation.
	if err := setMode(l.path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(l.path)
	if err != nil {
		return nil, err
	}
	l.socket = info
	if err := private(l.socket, fs.ModeSocket, 0o600); err != nil {
		return nil, fmt.Errorf("native HTTP: socket: %w", err)
	}
	if err := checkACL(l.path); err != nil {
		return nil, err
	}
	stat := l.socket.Sys().(*syscall.Stat_t)
	rootInfo, err := r.Stat(".")
	if err != nil {
		return nil, err
	}
	rootStat := rootInfo.Sys().(*syscall.Stat_t)
	d := discovery{Format: 1, Root: r.Name(), Instance: cfg.Instance, Version: cfg.Version, Generation: name,
		RootDevice: uint64(rootStat.Dev), RootInode: uint64(rootStat.Ino),
		Socket: l.path, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	if err := publish(l.state, r, d); err != nil {
		return nil, err
	}
	return l, nil
}

func validConfig(cfg Config) error {
	if cfg.Root == "" || !validText(cfg.Instance) || !validText(cfg.Version) {
		return errors.New("native HTTP: root, installation identity and version are required")
	}
	return nil
}

func validText(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n")
}

func socketPath(root *os.Root, cfg Config, generation string, create bool) (string, error) {
	name := "native-" + generation + ".sock"
	path := filepath.Join(root.Name(), "state", name)
	if len(path) <= maxSocketPath {
		return path, nil
	}
	parent := cfg.RuntimeDir
	if parent == "" {
		parent = "/tmp"
	}
	// Resolve only the runtime parent (on macOS /tmp is /private/tmp).
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	if err := trustedAncestors(filepath.Join(parent, "unused")); err != nil {
		return "", err
	}
	base := filepath.Join(parent, fmt.Sprintf("hnt-%d", os.Geteuid()))
	if create {
		if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	b, err := openPrivateRoot(base)
	if err != nil {
		return "", fmt.Errorf("native HTTP: runtime directory: %w", err)
	}
	defer b.Close()
	hash := sha256.Sum256([]byte(root.Name() + "\x00" + cfg.Instance))
	d, err := privateDir(b, hex.EncodeToString(hash[:12]), create)
	if err != nil {
		return "", err
	}
	defer d.Close()
	path = filepath.Join(d.Name(), name)
	if len(path) > maxSocketPath {
		return "", errors.New("native HTTP: private runtime socket path is too long")
	}
	return path, nil
}

func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Accept refuses foreign or unverifiable peers before HTTP reads any bytes.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if checkPeer(conn) == nil {
			return conn, nil
		}
		conn.Close()
	}
}

// Close checks the socket identity before removing its pathname. Namespace
// mutation by another process of the same UID is outside this OS-user
// boundary; it must be serialized by the embedding daemon. Discovery
// remains as a stale binding; a client must successfully dial it, and a new
// daemon may replace it only after acquiring the native lease.
func (l *Listener) Close() error {
	l.once.Do(func() {
		if l.ln != nil {
			l.closeErr = l.ln.Close()
			if current, err := os.Lstat(l.path); err == nil && l.socket != nil && os.SameFile(current, l.socket) {
				l.closeErr = errors.Join(l.closeErr, os.Remove(l.path))
			}
		}
		if l.lease != nil {
			l.closeErr = errors.Join(l.closeErr, l.lease.Close())
		}
		if l.state != nil {
			l.closeErr = errors.Join(l.closeErr, l.state.Close())
		}
		if l.root != nil {
			l.closeErr = errors.Join(l.closeErr, l.root.Close())
		}
	})
	return l.closeErr
}
