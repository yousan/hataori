package host

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// HostLog is the host's own record, in the state directory, for a person:
// when it started and ended, how its command ended, every connection it
// dropped and why, and a signal that ends it. A panic or fatal error of the
// host is written to it by the Go runtime. A host killed with SIGKILL
// leaves a start without an end.
const HostLog = "host.log"

// maxDrops bounds the dropped connections recorded by one host.
const maxDrops = 100

type hostLog struct {
	mu    sync.Mutex
	f     *os.File
	drops int
}

// openHostLog opens the host log of dir, and has the runtime write a crash
// of this process there too. It returns nil when the file cannot be opened:
// the host runs without it.
func openHostLog(dir string) *hostLog {
	f, err := os.OpenFile(dir+"/"+HostLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	debug.SetCrashOutput(f, debug.CrashOptions{})
	return &hostLog{f: f}
}

func (l *hostLog) printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

// dropped records a connection the host dropped, up to maxDrops.
func (l *hostLog) dropped(why string, err error) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.drops++
	n := l.drops
	l.mu.Unlock()
	switch {
	case n < maxDrops:
		l.printf("dropped a connection: %s: %v", why, err)
	case n == maxDrops:
		l.printf("dropped a connection: %s: %v (further drops are not recorded)", why, err)
	}
}

// recordSignals records a signal that ends the host, then lets it end the
// host as it would have. SIGINT and SIGTERM are not among them: `hataori`
// catches those for every command, so they do not end a host.
func (l *hostLog) recordSignals() {
	if l == nil {
		return
	}
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGHUP, syscall.SIGQUIT} {
		if !signal.Ignored(s) { // one ignored from the start ends nothing
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	go func() {
		s := <-ch
		l.printf("ended by signal %v", s)
		signal.Reset(sigs...)
		syscall.Kill(os.Getpid(), s.(syscall.Signal))
	}()
}
