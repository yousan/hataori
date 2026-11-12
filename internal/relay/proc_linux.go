package relay

import "syscall"

// procAttr puts the login command in a group of its own, and kills it if
// the daemon dies: a daemon restart ends a login in progress.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
