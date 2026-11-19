package plugin

import "syscall"

// procAttr puts a plugin in a process group of its own. Pdeathsig kills it
// when the daemon dies without stopping it.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
