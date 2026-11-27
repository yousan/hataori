//go:build linux

package worker

import (
	"math"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func bootID() (string, error) {
	// This is the kernel's boot UUID, not a process lookup or ownership proof.
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

func memoryLoad() (uint64, float64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, 0, err
	}
	unit := uint64(info.Unit)
	if unit == 0 {
		unit = 1
	}
	if uint64(info.Freeram) > math.MaxUint64/unit {
		return 0, 0, unix.EOVERFLOW
	}
	// Free RAM is conservative; reclaimable caches are not counted as room.
	return uint64(info.Freeram) * unit, float64(info.Loads[0]) / 65536 / float64(runtime.NumCPU()), nil
}
