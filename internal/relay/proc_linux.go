package relay

import "syscall"

// procAttr puts the login command in a group of its own. Pdeathsig kills it
// when the daemon dies, so a daemon restart ends a login in progress.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// useGuard is false because Pdeathsig would kill a guard and leave its child.
func useGuard() bool { return false }
