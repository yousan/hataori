package worker

import (
	"math"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
	"golang.org/x/sys/unix"
)

func resourceTelemetry(root string, now time.Time) workerproto.Telemetry {
	unknown := workerproto.Telemetry{ObservedMS: now.UnixMilli(), ValidMS: 10000}
	memory, load, err := memoryLoad()
	if err != nil {
		return unknown
	}
	var disk unix.Statfs_t
	if unix.Statfs(root, &disk) != nil || disk.Bsize <= 0 || uint64(disk.Bavail) > math.MaxUint64/uint64(disk.Bsize) {
		return unknown
	}
	return workerproto.Telemetry{ObservedMS: now.UnixMilli(), ValidMS: 10000, Known: true, MemoryFree: memory, DiskFree: uint64(disk.Bavail) * uint64(disk.Bsize), Load: load}
}
