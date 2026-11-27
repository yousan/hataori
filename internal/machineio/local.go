package machineio

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// Local preserves the direct local host boundary. It never accepts a worker
// pathname or PID; construction is restricted to the main's session directory.
type Local struct {
	Dir  string
	Host *holder.Host
}

func (l *Local) Metadata(ctx context.Context) (Metadata, error) {
	s, ok, err := host.ReadSpec(l.Dir)
	if err != nil {
		return Metadata{}, err
	}
	if !ok {
		return Metadata{}, &ReadError{State: "missing"}
	}
	return Metadata{Terminal: s.Terminal}, nil
}

func generation(st os.FileInfo) string {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("f-%x-%x", uint64(s.Dev), uint64(s.Ino))
	}
	return ""
}

func (l *Local) path(name string) string {
	if name == "" {
		return filepath.Join(l.Dir, host.EventsFile)
	}
	return name // only an adapter's locally followed file
}

func (l *Local) Source(ctx context.Context, name string) (Source, error) {
	f, err := os.Open(l.path(name))
	if err != nil {
		return Source{}, readError(err)
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil {
		return Source{}, readError(err)
	}
	if !s.Mode().IsRegular() || generation(s) == "" {
		return Source{}, &ReadError{State: "unreadable"}
	}
	return Source{Name: name, Handle: generation(s), Generation: generation(s)}, nil
}

func readError(err error) error {
	if os.IsNotExist(err) {
		return &ReadError{State: "missing"}
	}
	return &ReadError{State: "unreadable"}
}

func (l *Local) Read(ctx context.Context, src Source, off int64, limit int) (Range, error) {
	if off < 0 || limit <= 0 || limit > MaxRead {
		return Range{}, fmt.Errorf("session read: invalid range")
	}
	f, err := os.Open(l.path(src.Name))
	if err != nil {
		return Range{}, readError(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Range{}, readError(err)
	}
	if !st.Mode().IsRegular() {
		return Range{}, &ReadError{State: "unreadable"}
	}
	if generation(st) != src.Generation {
		return Range{}, &ReadError{State: "replaced"}
	}
	if st.Size() < off {
		return Range{}, &ReadError{State: "truncated"}
	}
	n := min(int64(limit), st.Size()-off)
	b := make([]byte, n)
	got, err := f.ReadAt(b, off)
	if err != nil && err != io.EOF {
		return Range{}, readError(err)
	}
	if int64(got) != n {
		return Range{}, &ReadError{State: "truncated"}
	}
	after, err := f.Stat()
	if err != nil {
		return Range{}, readError(err)
	}
	if after.Size() < st.Size() {
		return Range{}, &ReadError{State: "truncated"}
	}
	return Range{Source: src, Offset: off, Data: b}, nil
}

func (l *Local) Write(ctx context.Context, key, turn string, data [][]byte) error {
	return l.Host.Write(ctx, l.Host.Window(l.Dir), data)
}
func (l *Local) Answer(ctx context.Context, key, turn, request string, data []byte) error {
	c, err := host.DialContext(ctx, l.Dir)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Answer(request, data)
}
func (l *Local) Waiting(ctx context.Context) ([]host.WaitingCall, error) {
	c, err := host.DialContext(ctx, l.Dir)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Waiting()
}
func (l *Local) Alive(ctx context.Context) (holder.Liveness, error) {
	return l.Host.Alive(ctx, l.Host.Window(l.Dir))
}
func (l *Local) End(ctx context.Context, key string, interrupt, terminate time.Duration) (holder.StopResult, error) {
	return l.Host.Stop(ctx, l.Host.Window(l.Dir), interrupt, terminate)
}
