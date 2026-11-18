//go:build !linux

package relay

import "syscall"

// procAttr puts the login command in a group of its own. Outside Linux
// the guard ends it when the daemon dies, when configured.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// useGuard enables the guard when configured.
func useGuard() bool { return true }
