package workerssh

import (
	"bytes"
	"sync"
)

// Cause is a bounded diagnostic category, never raw OpenSSH/provider output.
type Cause string

const (
	Connecting     Cause = "connecting"
	Authentication Cause = "authentication"
	HostKey        Cause = "host_key"
	Forward        Cause = "forward"
	Binding        Cause = "binding"
	Protocol       Cause = "protocol"
	Reconciliation Cause = "reconciliation"
	Transport      Cause = "transport"
	Closed         Cause = "closed"
)

type Error struct{ Cause Cause }

func (e *Error) Error() string  { return "worker SSH unavailable: " + string(e.Cause) }
func unavailable(c Cause) error { return &Error{Cause: c} }

// diagnostic keeps only a bounded tail. It is classified, never exposed in
// errors/status/logs: SSH output can contain destinations and authentication data.
type diagnostic struct {
	mu   sync.Mutex
	tail []byte
}

func (d *diagnostic) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	const limit = 8192
	n := len(p)
	if len(p) >= limit {
		d.tail = append(d.tail[:0], p[len(p)-limit:]...)
	} else {
		if len(d.tail)+len(p) > limit {
			d.tail = append(d.tail[:0], d.tail[len(d.tail)+len(p)-limit:]...)
		}
		d.tail = append(d.tail, p...)
	}
	return n, nil
}
func (d *diagnostic) cause(fallback Cause) Cause {
	d.mu.Lock()
	defer d.mu.Unlock()
	// These phrases are covered by captured OpenSSH failures under testdata.
	for _, rule := range []struct {
		phrase string
		cause  Cause
	}{
		{"Host key verification failed", HostKey},
		{"Permission denied", Authentication},
		{"open failed: connect failed", Forward},
	} {
		if bytes.Contains(d.tail, []byte(rule.phrase)) {
			return rule.cause
		}
	}
	return fallback
}

func (d *diagnostic) snapshot() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.tail...)
}
