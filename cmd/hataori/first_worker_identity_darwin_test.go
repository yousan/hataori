package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func firstWorkerBirth(pid int) (string, bool, error) {
	p, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if errors.Is(err, syscall.ESRCH) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(p) == 0 {
		return "", true, nil
	}
	if len(p) != 1 || int(p[0].Proc.P_pid) != pid {
		return "", false, syscall.EIO
	}
	return fmt.Sprint(p[0].Proc.P_starttime), p[0].Proc.P_stat == 5, nil
}

func firstWorkerChildren(pid, group int) ([]int, error) {
	// Every managed connection starts a private group; inherited members are
	// queried only for exit evidence, and are never signalled by this query.
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", group)
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, p := range processes {
		if int(p.Eproc.Ppid) == pid {
			ids = append(ids, int(p.Proc.P_pid))
		}
	}
	return ids, nil
}

func firstWorkerSocketPID(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
func firstWorkerParent(pid int) (int, error) {
	p, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, os.ErrNotExist
	}
	if len(p) != 1 || int(p[0].Proc.P_pid) != pid {
		return 0, syscall.EIO
	}
	return int(p[0].Eproc.Ppid), nil
}
