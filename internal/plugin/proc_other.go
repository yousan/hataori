//go:build !linux

package plugin

import "syscall"

// procAttr puts a plugin in a process group of its own.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
