package main

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
)

// Read-only evidence, never cleanup authority. Only enumerate descendants of
// the still-unreaped creator record, and preserve the kernel birth counter.
func firstWorkerBirth(pid int) (string, bool, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if os.IsNotExist(err) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	end := bytes.LastIndexByte(b, ')')
	if end < 0 {
		return "", false, os.ErrInvalid
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return "", false, os.ErrInvalid
	}
	return fields[19], fields[0] == "Z" || fields[0] == "X", nil
}
func firstWorkerChildren(pid, group int) ([]int, error) {
	base := "/proc/" + strconv.Itoa(pid) + "/task/"
	tasks, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var ids []int
	for _, task := range tasks {
		b, err := os.ReadFile(base + task.Name() + "/children")
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, field := range strings.Fields(string(b)) {
			id, err := strconv.Atoi(field)
			if err != nil {
				return nil, err
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func firstWorkerSocketPID(fd uintptr) (int, error) {
	c, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(c.Pid), nil
}
func firstWorkerParent(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(b, ')')
	if end < 0 {
		return 0, os.ErrInvalid
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 2 {
		return 0, os.ErrInvalid
	}
	return strconv.Atoi(f[1])
}
