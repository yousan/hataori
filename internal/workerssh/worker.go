package workerssh

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/worker"
	"golang.org/x/sys/unix"
)

type Worker struct {
	mu       sync.Mutex
	config   Config
	options  Options
	ctx      context.Context
	cancel   context.CancelFunc
	lock     *os.File
	status   Status
	changed  chan struct{}
	done     chan struct{}
	wake     chan struct{}
	current  *generation
	sequence uint64
}

type generation struct {
	sequence          uint64
	dir, ctl, forward string
	dirInfo           os.FileInfo
	sockets           map[string]os.FileInfo
	command           *exec.Cmd
	ended             chan struct{}
	diagnostic        diagnostic
	streams           map[*Stream]bool // Worker.mu
}

func (w *Worker) Status() Status { w.mu.Lock(); defer w.mu.Unlock(); return w.status }

// WaitReady waits only when explicitly requested; Open itself never waits for
// authentication/reconnect. Placement can inspect Status without blocking.
func (w *Worker) WaitReady(ctx context.Context) error {
	for {
		w.mu.Lock()
		status, changed := w.status, w.changed
		w.mu.Unlock()
		if status.Ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
			return unavailable(Closed)
		case <-changed:
		}
	}
}

// Changed broadcasts status changes; callers re-read Status after it closes.
func (w *Worker) Changed() <-chan struct{} { w.mu.Lock(); defer w.mu.Unlock(); return w.changed }

func (w *Worker) publish(g *generation, ready bool, cause Cause) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.publishLocked(g, ready, cause)
}

// The exit watcher invalidates a live connection immediately, but must preserve
// the specific refusal already recorded before intentional teardown.
func (w *Worker) masterEnded(g *generation) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == g && (w.status.Ready || w.status.Cause == Connecting) {
		w.publishLocked(g, false, g.diagnostic.cause(Transport))
	}
}

func (w *Worker) publishLocked(g *generation, ready bool, cause Cause) {
	if g != nil && g.sequence != w.sequence {
		return
	}
	if w.ctx.Err() != nil {
		ready, cause = false, Closed
	}
	if ready && g != nil {
		select {
		case <-g.ended:
			ready, cause = false, g.diagnostic.cause(Transport)
		default:
		}
	}
	seq := w.sequence
	if g != nil {
		seq = g.sequence
	}
	if w.status.Ready == ready && w.status.Cause == cause && w.status.Generation == seq {
		return
	}
	w.status = Status{Ready: ready, Cause: cause, Generation: seq, Since: time.Now()}
	close(w.changed)
	w.changed = make(chan struct{})
}

// Open gives one independent duplex stream over this destination's shared SSH
// connection. It validates a fresh worker hello before returning. It never
// writes/retries a worker operation on behalf of its caller.
func (w *Worker) Open(ctx context.Context) (*Stream, error) {
	w.mu.Lock()
	g, ready, cause := w.current, w.status.Ready, w.status.Cause
	w.mu.Unlock()
	if !ready || g == nil {
		return nil, unavailable(cause)
	}
	select {
	case <-g.ended:
		w.masterEnded(g)
		return nil, unavailable(Transport)
	default:
	}
	stream, err := w.handshake(ctx, g)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		w.publish(g, false, errorCause(err))
		select {
		case w.wake <- struct{}{}:
		default:
		}
		return nil, err
	}
	w.mu.Lock()
	valid := w.current == g && w.status.Ready
	w.mu.Unlock()
	if !valid {
		stream.Close()
		return nil, unavailable(Transport)
	}
	return stream, nil
}

func errorCause(err error) Cause {
	var e *Error
	if errors.As(err, &e) {
		return e.Cause
	}
	return Transport
}

func (w *Worker) run() {
	defer close(w.done)
	defer func() { unix.Flock(int(w.lock.Fd()), unix.LOCK_UN); w.lock.Close() }()
	delay := w.options.BackoffMin
	for w.ctx.Err() == nil {
		g, err := w.connect()
		if err == nil {
			w.publish(g, true, "")
			delay = w.options.BackoffMin
			ticker := time.NewTicker(w.options.ProbeInterval)
		live:
			for {
				select {
				case <-w.ctx.Done():
					err = unavailable(Closed)
					break live
				case <-g.ended:
					err = unavailable(g.diagnostic.cause(Transport))
					break live
				case <-ticker.C:
				case <-w.wake:
				}
				if err = w.probe(g); err != nil {
					break live
				}
				w.publish(g, true, "")
			}
			ticker.Stop()
		}
		cause := errorCause(err)
		if w.ctx.Err() != nil {
			cause = Closed
		}
		w.publish(g, false, cause)
		w.teardown(g)
		if g != nil && w.options.capture != nil {
			w.options.capture(cause, g.diagnostic.snapshot())
		}
		if w.ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-w.ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if delay < w.options.BackoffMax {
			delay *= 2
			if delay > w.options.BackoffMax {
				delay = w.options.BackoffMax
			}
		}
	}
	w.publish(nil, false, Closed)
}

func masterArgs(control, destination string) []string {
	return []string{"-M", "-S", control, "-N", "-n", "-T",
		"-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no",
		"-o", "ClearAllForwardings=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "BatchMode=yes", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
		"-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "CheckHostIP=no",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no",
		"-o", "GSSAPIDelegateCredentials=no", "-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none",
		"-o", "AddKeysToAgent=no", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "--", destination}
}

// The master cleared all configured forwards. Mux commands must not read the
// operator config again and silently add its forwards/commands. -O requires the
// existing private master; it cannot fall back to a new authenticated session.
func muxArgs(control, destination, operation string, extra ...string) []string {
	args := []string{"-F", "/dev/null", "-S", control, "-o", "ControlMaster=no", "-o", "ClearAllForwardings=no", "-o", "ExitOnForwardFailure=yes", "-O", operation}
	args = append(args, extra...)
	return append(args, "--", destination)
}
func forwardPath(path string) string {
	return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(path)
}
func sshEnvironment() []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "LC_ALL" && key != "SSH_ASKPASS" && key != "SSH_ASKPASS_REQUIRE" {
			env = append(env, value)
		}
	}
	return append(env, "LC_ALL=C", "SSH_ASKPASS=", "SSH_ASKPASS_REQUIRE=never")
}

func (w *Worker) connect() (*generation, error) {
	ctx, cancel := context.WithTimeout(w.ctx, w.options.ConnectTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "hataori-workerssh-")
	if err != nil {
		return nil, unavailable(Transport)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, unavailable(Transport)
	}
	w.mu.Lock()
	w.sequence++
	seq := w.sequence
	w.mu.Unlock()
	g := &generation{sequence: seq, dir: dir, dirInfo: info, ctl: filepath.Join(dir, "control.sock"), forward: filepath.Join(dir, "resident.sock"), sockets: map[string]os.FileInfo{}, streams: map[*Stream]bool{}, ended: make(chan struct{})}
	w.publish(g, false, Connecting)
	command := exec.Command(w.options.SSH, masterArgs(g.ctl, w.config.Destination)...)
	command.Env = sshEnvironment()
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = &g.diagnostic
	// A ProxyCommand/helper can retain a stderr pipe after ssh exits. Bound
	// os/exec's copy wait without signalling any helper we do not own.
	command.WaitDelay = 250 * time.Millisecond
	if err := command.Start(); err != nil {
		return g, unavailable(Transport)
	}
	g.command = command
	w.mu.Lock()
	w.current = g
	w.mu.Unlock()
	go func() { command.Wait(); close(g.ended); w.masterEnded(g) }()
	for {
		if g.noteSocket(g.ctl) {
			if err := w.mux(ctx, g, "check"); err == nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return g, unavailable(g.diagnostic.cause(Transport))
		case <-g.ended:
			return g, unavailable(g.diagnostic.cause(Transport))
		case <-time.After(25 * time.Millisecond):
		}
	}
	remote := worker.SocketPath(w.config.Root)
	if err := w.mux(ctx, g, "forward", "-L", forwardPath(g.forward)+":"+forwardPath(remote)); err != nil {
		return g, err
	}
	if !g.noteSocket(g.forward) {
		return g, unavailable(Forward)
	}
	if err := w.probeContext(ctx, g); err != nil {
		return g, err
	}
	select {
	case <-g.ended:
		return g, unavailable(g.diagnostic.cause(Transport))
	default:
	}
	return g, nil
}

func (w *Worker) mux(ctx context.Context, g *generation, op string, extra ...string) error {
	command := exec.CommandContext(ctx, w.options.SSH, muxArgs(g.ctl, w.config.Destination, op, extra...)...)
	command.Env = sshEnvironment()
	command.Stdin = nil
	command.Stdout = io.Discard
	var diagnostic diagnostic
	command.Stderr = &diagnostic
	command.WaitDelay = 250 * time.Millisecond
	if err := command.Run(); err != nil {
		return unavailable(diagnostic.cause(g.diagnostic.cause(Forward)))
	}
	return nil
}

func (g *generation) noteSocket(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	g.sockets[path] = info
	return true
}

func (w *Worker) teardown(g *generation) {
	if g == nil {
		return
	}
	w.mu.Lock()
	if w.current == g {
		w.current = nil
	}
	streams := make([]*Stream, 0, len(g.streams))
	for s := range g.streams {
		streams = append(streams, s)
	}
	w.mu.Unlock()
	for _, s := range streams {
		s.Close()
	}
	if g.command != nil {
		// Signal only the exact ssh process started by this generation. No stored
		// PID, process-name lookup, foreign control socket or remote command stops it.
		g.command.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-g.ended:
			timer.Stop()
		case <-timer.C:
			g.command.Process.Kill()
			<-g.ended
		}
	}
	g.cleanup()
}

func (g *generation) cleanup() {
	for path, created := range g.sockets {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 && os.SameFile(info, created) {
			os.Remove(path)
		}
	}
	// Only this generation's new directory, only if it still has its inode;
	// Remove refuses when any unrecognised file remains. Never RemoveAll.
	if info, err := os.Lstat(g.dir); err == nil && info.IsDir() && os.SameFile(info, g.dirInfo) {
		os.Remove(g.dir)
	}
}
