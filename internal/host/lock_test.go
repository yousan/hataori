package host

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// A process the daemon starts while Running probes the lock has a copy of
// the probe's descriptor until it execs. The lock is free all the same
// once Running returned false, so a host can take it at once.
func TestRunningLeavesTheLockFreeWhileAChildHasACopy(t *testing.T) {
	dir := t.TempDir()
	probed = func(f *os.File) {
		// This child keeps its copy for as long as it runs, as a child
		// does for the moment between its fork and its exec.
		child := exec.Command("sleep", "60")
		child.ExtraFiles = []*os.File{f}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	}
	t.Cleanup(func() { probed = nil })
	if Running(dir) {
		t.Fatal("Running: a host holds an empty state directory")
	}
	probed = nil
	f, err := lock(dir)
	if errors.Is(err, ErrHostRunning) {
		t.Fatal("the lock is still held by the child's copy of Running's probe")
	}
	if err != nil {
		t.Fatal(err)
	}
	unlock(f)
	if Running(dir) {
		t.Fatal("Running after the lock was released: a host holds it")
	}
}
