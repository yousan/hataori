package nativehttp

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

type changedStat struct {
	fs.FileInfo
	stat any
}

func (s changedStat) Sys() any { return s.stat }

// These variations exercise unreadable/foreign metadata, not an executed
// cross-user boundary. The subprocess fixture supplies the latter only on
// a separately authorized privileged runner.
func TestPrivateRefusesForeignOrUnknownOwner(t *testing.T) {
	cfg := fixture(t)
	info, err := os.Lstat(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	foreign := *info.Sys().(*syscall.Stat_t)
	foreign.Uid++
	for _, stat := range []any{nil, &foreign} {
		if err := private(changedStat{info, stat}, fs.ModeDir, 0o700); err == nil {
			t.Fatal("accepted foreign or unknown owner metadata")
		}
	}
}
