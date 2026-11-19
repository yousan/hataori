package plugin

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// bootID is when this boot began (kern.boottime).
func bootID() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}

// ownProcesses lists the processes that run as this OS user.
func ownProcesses() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		pids = append(pids, int(p.Proc.P_pid))
	}
	return pids, nil
}

// environ returns the environment in kern.procargs2's layout: argc, a
// 32-bit integer in the host's order; the executable's path and NULs up to
// the arguments; argc arguments, each ending with a NUL; then the
// environment, each entry ending with a NUL, up to an empty string. Only
// the environment is returned, so that an argument is never taken for an
// entry of it.
func environ(args []byte) ([]byte, bool) {
	if len(args) < 4 {
		return nil, false
	}
	argc := int(int32(binary.NativeEndian.Uint32(args)))
	b := args[4:]
	i := bytes.IndexByte(b, 0) // the executable's path
	if i < 0 || argc < 0 {
		return nil, false
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	for range argc {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, false
		}
		b = b[i+1:]
	}
	if end := bytes.Index(b, []byte{0, 0}); end >= 0 {
		b = b[:end+1]
	}
	return b, true
}

// procOf returns process pid's start time and real user, or ok false when
// there is no such process.
func procOf(pid int) (start unix.Timeval, uid uint32, ok bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil || len(procs) != 1 || int(procs[0].Proc.P_pid) != pid {
		return start, 0, false
	}
	return procs[0].Proc.P_starttime, procs[0].Eproc.Pcred.P_ruid, true
}

// killIfOfRun kills process pid with SIGKILL when it is of this OS user and
// its environment (kern.procargs2) holds marker. macOS has no pidfd: the
// process's start time is read before its environment and again just
// before the signal, and a pid given to another process between the two
// reads changes it, so that process is not signalled. Only the moment
// between the second read and the signal is left.
func killIfOfRun(pid int, marker string) (bool, error) {
	start, uid, ok := procOf(pid)
	if !ok || uid != uint32(os.Getuid()) {
		return false, nil
	}
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return false, nil // gone, or not readable
	}
	if env, ok := environ(args); !ok || !hasEnv(env, marker) {
		return false, nil
	}
	if again, _, ok := procOf(pid); !ok || again != start {
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	// Waited for up to a second, so that the plugin does not start while it
	// still holds files.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if again, _, ok := procOf(pid); !ok || again != start {
			break
		}
	}
	return true, nil
}
