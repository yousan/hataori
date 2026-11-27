// Package machineio identifies session I/O by its owning machine and run.
// Paths returned in metadata are for an adapter's pure codec; a caller must
// never open them on a different machine.
package machineio

import (
	"context"
	"errors"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

const MaxRead = 64 << 10

// Source names a retained source within one Session. Generation changes are
// failures, never permission to resume at a new file's old offset.
type Source struct{ Name, Handle, Generation string }

type Range struct {
	Source Source
	Offset int64
	Data   []byte
}

// ReadError preserves the distinction between an empty read and lost history.
type ReadError struct{ State string }

func (e *ReadError) Error() string { return "session read: " + e.State }

var ErrUnknown = errors.New("session operation outcome is unknown")

type Metadata struct {
	Spec     agent.Spec
	Terminal bool
}

// Session is bound before any operation. Keys identify durable effects, not
// transport attempts; unknown writes must keep the caller's written marker.
type Session interface {
	Metadata(context.Context) (Metadata, error)
	Source(context.Context, string) (Source, error)
	Read(context.Context, Source, int64, int) (Range, error)
	Write(context.Context, string, string, [][]byte) error
	Answer(context.Context, string, string, string, []byte) error
	Waiting(context.Context) ([]host.WaitingCall, error)
	Alive(context.Context) (holder.Liveness, error)
	End(context.Context, string, time.Duration, time.Duration) (holder.StopResult, error)
}
