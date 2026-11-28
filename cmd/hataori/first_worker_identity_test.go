package main

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

type firstWorkerIdentity struct {
	pid   int
	birth string
}

func (o *firstWorkerProcesses) snapshot() []*firstWorkerProcess {
	o.mu.Lock()
	defer o.mu.Unlock()
	records := make([]*firstWorkerProcess, 0, len(o.records))
	for r := range o.records {
		records = append(records, r)
	}
	return records
}

type firstWorkerObservation struct {
	identities []firstWorkerIdentity
	err        error
}

// The single waiter observes the process tree before reaping the creator.
// No query can cross from a reaped child into a reused root PID.
func firstWorkerDescendants(records []*firstWorkerProcess) ([]firstWorkerIdentity, error) {
	var identities []firstWorkerIdentity
	for _, r := range records {
		reply := make(chan firstWorkerObservation, 1)
		select {
		case <-r.done:
			r.owner.mu.Lock()
			for identity := range r.observed {
				identities = append(identities, identity)
			}
			r.owner.mu.Unlock()
			continue
		case r.inspect <- reply:
		}
		observation := <-reply
		if observation.err != nil {
			return nil, observation.err
		}
		identities = append(identities, observation.identities...)
	}
	return identities, nil
}
func firstWorkerObserveDescendants(pid int) ([]firstWorkerIdentity, error) {
	return firstWorkerObserveDescendantsWith(pid, firstWorkerChildren)
}

func firstWorkerObserveDescendantsWith(pid int, children func(int, int) ([]int, error)) ([]firstWorkerIdentity, error) {
	var identities []firstWorkerIdentity
	var visit func(firstWorkerIdentity) error
	visit = func(parent firstWorkerIdentity) error {
		birth, ended, err := firstWorkerBirth(parent.pid)
		if err != nil {
			return err
		}
		if ended || birth != parent.birth {
			return nil
		}
		ids, err := children(parent.pid, pid)
		if err != nil {
			return err
		}
		for _, pid := range ids {
			birth, ended, err := firstWorkerBirth(pid)
			if err != nil {
				return err
			}
			actualParent, err := firstWorkerParent(pid)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
					continue
				}
				return err
			}
			parentBirth, parentEnded, err := firstWorkerBirth(parent.pid)
			if err != nil {
				return err
			}
			if parentEnded || parentBirth != parent.birth {
				return nil
			}
			if !ended && actualParent == parent.pid {
				identities = append(identities, firstWorkerIdentity{pid, birth})
				if err := visit(firstWorkerIdentity{pid, birth}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	birth, ended, err := firstWorkerBirth(pid)
	if err != nil {
		return nil, err
	}
	if ended {
		return nil, nil
	}
	if err := visit(firstWorkerIdentity{pid, birth}); err != nil {
		return nil, err
	}
	return identities, nil
}
func firstWorkerWaitExit(records []*firstWorkerProcess, children []firstWorkerIdentity) error {
	deadline := time.Now().Add(6 * time.Second)
	for _, r := range records {
		select {
		case <-r.done:
			if r.err != nil {
				return r.err
			}
		case <-time.After(time.Until(deadline)):
			return errors.New("fixture owned process still running")
		}
	}
	for time.Now().Before(deadline) {
		live := false
		for _, child := range children {
			birth, ended, err := firstWorkerBirth(child.pid)
			if err != nil {
				return err
			}
			if !ended && birth == child.birth {
				live = true
			}
		}
		if !live {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("fixture established SSH descendant still running")
}

func (f *firstWorker) kill() {
	f.t.Helper()
	f.ssh.freeze()
	records := f.ssh.processes.snapshot()
	descendants, err := firstWorkerDescendants(records)
	if err != nil {
		f.t.Fatal(err)
	}
	f.main.kill(f.t)
	helpers, err := f.ssh.cutGeneration()
	if err != nil {
		f.t.Fatal(err)
	}
	descendants = append(descendants, helpers...)
	records = f.ssh.processes.snapshot()
	if err := firstWorkerWaitExit(records, descendants); err != nil {
		f.t.Fatal(err)
	}
}
func (f *firstWorker) restart() { f.t.Helper(); f.kill(); f.main.start(f.t) }

func firstWorkerPeer(c *net.UnixConn) (firstWorkerIdentity, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return firstWorkerIdentity{}, err
	}
	var pid int
	var lookup error
	if err := raw.Control(func(fd uintptr) { pid, lookup = firstWorkerSocketPID(fd) }); err != nil {
		return firstWorkerIdentity{}, err
	}
	if lookup != nil {
		return firstWorkerIdentity{}, lookup
	}
	birth, ended, err := firstWorkerBirth(pid)
	if err != nil || ended {
		return firstWorkerIdentity{}, errors.New("fixture SSH peer identity unavailable")
	}
	return firstWorkerIdentity{pid, birth}, nil
}
