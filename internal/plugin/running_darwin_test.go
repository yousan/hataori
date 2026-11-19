package plugin

import "golang.org/x/sys/unix"

// running reports whether process pid runs: it exists and is no zombie.
func running(pid int) bool {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	const zombie = 5 // SZOMB
	return err == nil && len(procs) == 1 && int(procs[0].Proc.P_pid) == pid && procs[0].Proc.P_stat != zombie
}
