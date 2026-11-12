//go:build !linux

package relay

import "syscall"

// procAttr puts the login command in a group of its own. Outside Linux
// nothing ends it when the daemon dies; it ends when its input closes.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
