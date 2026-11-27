package workerssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/connect"
	"github.com/yousan/hataori/internal/worker"
	"github.com/yousan/hataori/internal/workerproto"
	"golang.org/x/sys/unix"
)

type Options struct {
	// capture is used only by isolated fixture capture tests, never production logs.
	capture func(Cause, []byte)

	// RunDir holds only private connection locks, never credentials/configuration.
	RunDir         string
	SSH            string // installed system OpenSSH; empty means lookup on PATH
	ConnectTimeout time.Duration
	ProbeTimeout   time.Duration
	ProbeInterval  time.Duration
	BackoffMin     time.Duration
	BackoffMax     time.Duration
}

type Observation struct {
	Health    workerproto.Health
	Inventory workerproto.Inventory
}

type Config struct {
	Destination string              // operator-configured SSH alias or [user@]host, not a URL/options
	Root        string              // operator-configured worker-local root, never a reported address
	Binding     workerproto.Binding // previously pinned installation binding
	// Expected reads receiver-owned main records. A nil callback accepts only
	// an empty resident inventory. Reports never register their own authority.
	Expected func(context.Context) ([]workerproto.Target, error)
	// Reconcile can persist/check main recovery before readiness. It must honor
	// its context and must not grant authority to an unregistered report.
	Reconcile func(context.Context, Observation) error
}

type Status struct {
	Ready      bool
	Cause      Cause
	Generation uint64
	Since      time.Time
}

type Pool struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	options Options
	workers map[string]*Worker
	closed  bool
}

func New(ctx context.Context, options Options) (*Pool, error) {
	if !filepath.IsAbs(options.RunDir) || filepath.Clean(options.RunDir) != options.RunDir {
		return nil, errors.New("worker SSH: private absolute run directory required")
	}
	if err := connect.PrivateDir(options.RunDir); err != nil {
		return nil, errors.New("worker SSH: run directory is not private")
	}
	if options.SSH == "" {
		var err error
		options.SSH, err = exec.LookPath("ssh")
		if err != nil {
			return nil, unavailable(Transport)
		}
	}
	if options.ConnectTimeout == 0 {
		options.ConnectTimeout = 10 * time.Second
	}
	if options.ProbeTimeout == 0 {
		options.ProbeTimeout = 2 * time.Second
	}
	if options.ProbeInterval == 0 {
		options.ProbeInterval = 3 * time.Second
	}
	if options.BackoffMin == 0 {
		options.BackoffMin = 250 * time.Millisecond
	}
	if options.BackoffMax == 0 {
		options.BackoffMax = 30 * time.Second
	}
	if options.ConnectTimeout < 100*time.Millisecond || options.ConnectTimeout > 30*time.Second || options.ProbeTimeout < 50*time.Millisecond || options.ProbeTimeout > 5*time.Second || options.ProbeInterval < 50*time.Millisecond || options.ProbeInterval > 10*time.Second || options.BackoffMin < 50*time.Millisecond || options.BackoffMax < options.BackoffMin || options.BackoffMax > 30*time.Second {
		return nil, errors.New("worker SSH: timeouts/backoff exceed supported bounds")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Pool{ctx: ctx, cancel: cancel, options: options, workers: map[string]*Worker{}}, nil
}

func validDestination(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, "/\\:") && !strings.ContainsFunc(s, unicode.IsSpace) && !strings.ContainsFunc(s, unicode.IsControl)
}
func validConfig(c Config) bool {
	socket := worker.SocketPath(c.Root)
	return validDestination(c.Destination) && filepath.IsAbs(c.Root) && filepath.Clean(c.Root) == c.Root && c.Root != "/" && utf8.ValidString(c.Root) && !strings.ContainsFunc(c.Root, unicode.IsControl) && !strings.ContainsAny(socket, "$%") && workerproto.ValidID(c.Binding.Main) && workerproto.ValidID(c.Binding.Worker)
}

// Worker returns the one connection owner for an exact configured destination.
// A conflicting root/installation cannot quietly create a second connection.
// Callers keep their first callbacks current through receiver-owned storage.
func (p *Pool) Worker(config Config) (*Worker, error) {
	if !validConfig(config) {
		return nil, errors.New("worker SSH: invalid destination/root/installation or unsupported socket expansion")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ctx.Err() != nil {
		return nil, unavailable(Closed)
	}
	if w := p.workers[config.Destination]; w != nil {
		if w.config.Root != config.Root || w.config.Binding != config.Binding {
			return nil, errors.New("worker SSH: destination already has a different registered worker")
		}
		return w, nil
	}
	sum := sha256.Sum256([]byte(config.Destination))
	lock, err := privateLock(filepath.Join(p.options.RunDir, hex.EncodeToString(sum[:])+".lock"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(p.ctx)
	w := &Worker{config: config, options: p.options, ctx: ctx, cancel: cancel, lock: lock, changed: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1), status: Status{Cause: Connecting, Since: time.Now()}}
	p.workers[config.Destination] = w
	go w.run()
	return w, nil
}

func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	workers := make([]*Worker, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	p.mu.Unlock()
	for _, w := range workers {
		<-w.done
	}
	return nil
}

func privateLock(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err == nil && !privateFile(info) {
		return nil, errors.New("worker SSH: lock is not a private owned regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, unavailable(Transport)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, unavailable(Transport)
	}
	opened, err := f.Stat()
	if err != nil || !privateFile(opened) || info != nil && !os.SameFile(info, opened) {
		f.Close()
		return nil, unavailable(Transport)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("worker SSH: destination connection is already owned")
	}
	return f, nil
}
func privateFile(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}
