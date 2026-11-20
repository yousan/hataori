package proctest

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Start returns when process pid started, by the wall clock, to within a
// clock tick. It returns false when there is no such process or its start
// cannot be read.
func Start(pid int) (time.Time, bool) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, false
	}
	// The command name, in parentheses, may hold spaces: the fields after
	// it start with the third, the state; the start is the 22nd.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return time.Time{}, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	// Ticks since boot, at USER_HZ, which is 100 for /proc.
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return time.Time{}, false
	}
	now := time.Now()
	since := time.Duration(ts.Nano()) - time.Duration(ticks)*(time.Second/100)
	return now.Add(-since), true
}

// Ended reports whether process pid has ended: it is gone, or a zombie
// its parent has not reaped yet.
func Ended(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return false
	}
	state := stat[end+2] // the field after the name: R, S, D, Z, X, ...
	return state == 'Z' || state == 'X'
}
