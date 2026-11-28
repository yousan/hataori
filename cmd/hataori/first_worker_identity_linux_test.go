package main

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	return firstWorkerChildrenIn("/proc", pid)
}

func firstWorkerChildrenIn(root string, pid int) ([]int, error) {
	base := root + "/" + strconv.Itoa(pid) + "/task/"
	tasks, err := os.ReadDir(base)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	fallback := os.IsNotExist(err) || len(tasks) == 0
	seen := map[int]bool{}
	var ids []int
	for _, task := range tasks {
		b, err := os.ReadFile(base + task.Name() + "/children")
		if os.IsNotExist(err) {
			fallback = true
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
	if fallback {
		return firstWorkerScanChildren(root, pid)
	}
	return ids, nil
}

// Some kernels omit task/children. Scan only numeric process metadata and
// filter by parent; the caller rechecks parent and birth identities while its
// creator remains unreaped. These candidates never grant signal authority.
func firstWorkerScanChildren(root string, parent int) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		actualParent, err := firstWorkerParentIn(root, pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if actualParent == parent {
			ids = append(ids, pid)
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
	return firstWorkerParentIn("/proc", pid)
}

func firstWorkerParentIn(root string, pid int) (int, error) {
	b, err := os.ReadFile(root + "/" + strconv.Itoa(pid) + "/stat")
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

func TestFirstWorkerProcChildrenFallback(t *testing.T) {
	for _, missing := range []string{"task", "children", "one-task-children"} {
		t.Run(missing, func(t *testing.T) {
			owner := newFirstWorkerProcesses()
			directory := t.TempDir()
			null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			record, err := owner.start(os.Args[0], []string{"first-worker-tree", directory, "2"}, os.Environ(), []*os.File{null, null, null})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := owner.close(); err != nil {
					t.Error(err)
				}
				if err := firstWorkerWaitExit([]*firstWorkerProcess{record}, nil); err != nil {
					t.Error(err)
				}
			})
			pids := []int{os.Getpid()}
			eventually(t, "all fallback fixture PID markers complete", 5*time.Second, func() bool {
				ready := []int{os.Getpid()}
				for _, depth := range []string{"2", "1", "0"} {
					b, err := os.ReadFile(filepath.Join(directory, depth))
					if err != nil || !bytes.HasSuffix(b, []byte("\n")) {
						return false
					}
					pid, err := strconv.Atoi(strings.TrimSuffix(string(b), "\n"))
					if err != nil || pid <= 1 {
						return false
					}
					ready = append(ready, pid)
				}
				pids = ready
				return true
			})
			// Only this fixture's processes and the test parent appear in the
			// private proc view. Stat links read their actual kernel metadata;
			// missing interfaces are simulated without scanning shared processes.
			view := t.TempDir()
			if pids[1] != record.process.Pid {
				t.Fatal("fallback tree root is not the creator child")
			}
			for _, pid := range pids {
				entry := filepath.Join(view, strconv.Itoa(pid))
				if err := os.Mkdir(entry, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/proc/"+strconv.Itoa(pid)+"/stat", filepath.Join(entry, "stat")); err != nil {
					t.Fatal(err)
				}
				if missing != "task" {
					task := filepath.Join(entry, "task", strconv.Itoa(pid))
					if err := os.MkdirAll(task, 0700); err != nil {
						t.Fatal(err)
					}
					if missing == "one-task-children" {
						if err := os.WriteFile(filepath.Join(task, "children"), nil, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(filepath.Join(entry, "task", "missing"), 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			children, err := firstWorkerObserveDescendantsWith(record.process.Pid, func(pid, group int) ([]int, error) {
				return firstWorkerChildrenIn(view, pid)
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 2 {
				t.Fatalf("fallback descendant tree incomplete: %d", len(children))
			}
			seen := map[int]bool{}
			for _, child := range children {
				seen[child.pid] = true
			}
			if !seen[pids[2]] || !seen[pids[3]] || seen[os.Getpid()] {
				t.Fatal("fallback included foreign process or lost owned descendants")
			}
			// Unreadable candidate metadata must fail closed, not supply an
			// empty successful observation. This is only a private fake entry.
			if err := os.MkdirAll(filepath.Join(view, "999999999", "stat"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := firstWorkerChildrenIn(view, record.process.Pid); err == nil {
				t.Fatal("fallback accepted unreadable candidate metadata")
			}
			if err := owner.close(); err != nil {
				t.Fatal(err)
			}
			if err := firstWorkerWaitExit([]*firstWorkerProcess{record}, children); err != nil {
				t.Fatal(err)
			}
		})
	}
}
