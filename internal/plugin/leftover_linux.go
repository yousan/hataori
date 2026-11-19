package plugin

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// bootID is this boot's id, which the kernel makes anew at each boot.
func bootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

// groupMembers lists the processes whose process group is pgid.
func groupMembers(pgid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // gone
		}
		// After the command, which is in parentheses and may hold any byte:
		// state, ppid, pgrp.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) > 2 && f[2] == strconv.Itoa(pgid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// killIfOfRun kills process pid with SIGKILL when it is of this OS user and
// its environment holds marker. The process is held by a pidfd opened
// before it is checked: if pid is given to another process meanwhile, the
// check may read that one, but the signal goes to the process the pidfd
// holds, which is then gone, and reaches nobody.
func killIfOfRun(pid int, marker string) (bool, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pidfd: %w", err) // without one, nothing is signalled
	}
	defer unix.Close(fd)
	st, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return false, nil // gone
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return false, nil
	}
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil || !hasEnv(env, marker) {
		return false, nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
