package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// processStart returns when process pid started, by the wall clock. It
// returns false when there is no such process or its start cannot be read.
func processStart(pid int) (time.Time, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return time.Time{}, false
	}
	return time.Unix(k.Proc.P_starttime.Unix()), true
}

// processEnded reports whether process pid has ended: it is gone, or a
// zombie its parent has not reaped yet (SZOMB in <sys/proc.h>).
func processEnded(pid int) bool {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return true
	}
	const zombie = 5
	return k.Proc.P_stat == zombie
}
