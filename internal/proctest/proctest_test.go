package proctest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// sleeper starts a process that leads its own group, as a host or an agent
// does, and returns it and a channel that gets its end.
func sleeper(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done
}

// Kill signals a pid only while it is the process its pid file was
// written for: one that ended early leaves its pid file, and the pid may
// name another process by the time the test ends.
func TestKillOnlyTheProcessThePIDFileWasWrittenFor(t *testing.T) {
	dir := t.TempDir()
	// Written after its process started: it is that process's.
	agent, agentDone := sleeper(t)
	ours := filepath.Join(dir, "command.pid")
	if err := os.WriteFile(ours, []byte(strconv.Itoa(agent.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stale: written 50 ms before its pid went to a process the test did
	// not start as the host or the agent.
	other, otherDone := sleeper(t)
	stale := filepath.Join(dir, "host.pid")
	if err := os.WriteFile(stale, []byte(strconv.Itoa(other.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	start, ok := Start(other.Process.Pid)
	if !ok {
		t.Fatal("the other process's start cannot be read")
	}
	if err := os.Chtimes(stale, start.Add(-50*time.Millisecond), start.Add(-50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	Kill(ours)
	Kill(stale)
	select {
	case <-agentDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the process its own pid file names was not killed")
	}
	select {
	case err := <-otherDone:
		t.Fatalf("the process a stale pid file names was signalled: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if Ended(other.Process.Pid) {
		t.Fatal("Ended: the other process runs")
	}
}
