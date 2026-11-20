// Package proctest is for tests: it kills the processes a test's hosts and
// agents recorded in pid files, and only those.
//
// A host or agent that ended early leaves its pid file, and by the time a
// test cleans up, the pid may belong to another process, of another test
// or program, which must not be signalled. So a pid is signalled only while
// it is still the process its file was written for, or when no process has
// it any more.
package proctest

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// leeway is how much later than its pid file a process may seem to have
// started: a file's time is a few milliseconds coarse, and Linux gives a
// start in ticks of 10. A pid given to another process within it would
// still be signalled.
const leeway = 20 * time.Millisecond

// PID returns the pid that the pid file at path names, and whether it may
// be signalled: only while the pid is still the process the file was
// written for, or no process has it any more. When that cannot be told,
// it may not.
func PID(path string) (int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		// Gone. A new process never gets the id of a process group that
		// still has members, so a group it left behind is still its own.
		return pid, true
	}
	start, ok := Start(pid)
	if !ok {
		return 0, false
	}
	// The file is written once the process runs: a host writes its own,
	// and its agent's right after starting it. A process that started
	// after that only has the same number.
	return pid, !start.After(st.ModTime().Add(leeway))
}

// Kill sends SIGKILL to what the pid file at path names, when PID allows
// it: the process group the pid leads, or the process alone when it leads
// none. A pid that is gone gets its group killed, for what it left in it.
func Kill(path string) {
	pid, ok := PID(path)
	if !ok {
		return
	}
	if pg, err := syscall.Getpgid(pid); err == nil && pg != pid {
		syscall.Kill(pid, syscall.SIGKILL)
		return
	}
	syscall.Kill(-pid, syscall.SIGKILL)
}
