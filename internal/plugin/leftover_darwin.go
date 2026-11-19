package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"

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

// groupMembers lists the processes whose process group is pgid.
func groupMembers(pgid int) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		pids = append(pids, int(p.Proc.P_pid))
	}
	return pids, nil
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
	if err != nil || len(args) < 4 {
		return false, nil // gone, or not readable
	}
	// argc, the executable's path, then argv and the environment, each
	// ending with a NUL.
	if !hasEnv(bytes.TrimLeft(args[4:], "\x00"), marker) {
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
	return true, nil
}
