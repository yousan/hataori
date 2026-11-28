package main

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var errFirstWorkerForeignProcess = errors.New("first-worker fixture: process does not belong to this owner")

// A creator keeps the original Process and a private record, never a pidfile or
// a process-name match. Only supervise calls Wait4 or Signal. Until Wait4 reaps
// this child, even an exited child reserves its PID; after reaping no signal is
// sent. This avoids a Signal/concurrent-Wait PID reuse window on both platforms.
type firstWorkerProcesses struct {
	mu      sync.Mutex
	closed  bool
	records map[*firstWorkerProcess]bool
}
type firstWorkerProcess struct {
	owner    *firstWorkerProcesses
	process  *os.Process
	stop     chan struct{}
	once     sync.Once
	done     chan struct{}
	status   int
	err      error
	signals  atomic.Int64
	inspect  chan chan firstWorkerObservation
	observed map[firstWorkerIdentity]bool
}

func newFirstWorkerProcesses() *firstWorkerProcesses {
	return &firstWorkerProcesses{records: map[*firstWorkerProcess]bool{}}
}
func (o *firstWorkerProcesses) start(path string, args, env []string, files []*os.File) (*firstWorkerProcess, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, errors.New("first-worker fixture: owner is closed")
	}
	p, err := os.StartProcess(path, append([]string{path}, args...), &os.ProcAttr{Env: env, Files: files, Sys: &syscall.SysProcAttr{Setpgid: true}})
	if err != nil {
		return nil, err
	}
	r := &firstWorkerProcess{owner: o, process: p, stop: make(chan struct{}), done: make(chan struct{}), status: 255, inspect: make(chan chan firstWorkerObservation), observed: map[firstWorkerIdentity]bool{}}
	o.records[r] = true
	go r.supervise()
	return r, nil
}
func (o *firstWorkerProcesses) stop(r *firstWorkerProcess) error {
	o.mu.Lock()
	owned := r != nil && r.owner == o && o.records[r]
	o.mu.Unlock()
	if !owned {
		return errFirstWorkerForeignProcess
	}
	r.once.Do(func() { close(r.stop) })
	return nil
}
func (r *firstWorkerProcess) supervise() {
	defer close(r.done)
	defer r.process.Release()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var deadline time.Time
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(r.process.Pid, &status, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			r.err = err
			return
		} // ECHILD never authorizes a signal.
		if pid == r.process.Pid {
			if status.Exited() {
				r.status = status.ExitStatus()
			} else if status.Signaled() {
				r.status = 128 + int(status.Signal())
			}
			return
		}
		select {
		case reply := <-r.inspect:
			identities, err := firstWorkerObserveDescendants(r.process.Pid)
			r.owner.mu.Lock()
			for _, identity := range identities {
				r.observed[identity] = true
			}
			all := make([]firstWorkerIdentity, 0, len(r.observed))
			for identity := range r.observed {
				all = append(all, identity)
			}
			r.owner.mu.Unlock()
			reply <- firstWorkerObservation{all, err}
		case <-r.stop:
			if deadline.IsZero() {
				// The private record and creator remain live, and this single waiter
				// has not reaped the child. No externally supplied PID is consulted.
				r.owner.mu.Lock()
				owned := r.owner.records[r] && r.process != nil
				r.owner.mu.Unlock()
				if !owned {
					r.err = errFirstWorkerForeignProcess
					return
				}
				r.signals.Add(1)
				if err := r.process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
					r.err = err
				}
				deadline = time.Now().Add(2 * time.Second)
			} else if time.Now().After(deadline) {
				r.signals.Add(1)
				if err := r.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					r.err = err
				}
				deadline = time.Now().Add(2 * time.Second)
			}
			<-ticker.C
		case <-ticker.C:
		}
	}
}
func (o *firstWorkerProcesses) close() error {
	o.mu.Lock()
	o.closed = true
	records := make([]*firstWorkerProcess, 0, len(o.records))
	for r := range o.records {
		records = append(records, r)
	}
	o.mu.Unlock()
	var errs []error
	for _, r := range records {
		if err := o.stop(r); err != nil {
			errs = append(errs, err)
		}
	}
	deadline := time.After(6 * time.Second)
	for _, r := range records {
		select {
		case <-r.done:
			if r.err != nil {
				errs = append(errs, r.err)
			}
		case <-deadline:
			return errors.Join(append(errs, errors.New("first-worker fixture: child termination not confirmed"))...)
		}
	}
	return errors.Join(errs...)
}
