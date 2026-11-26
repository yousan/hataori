package core

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

type attachedRunner struct {
	Runner
	mu      sync.Mutex
	pids    []int
	err     error
	queried []int64
}

func (r *attachedRunner) AttachedPIDs(_ context.Context, id int64) ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queried = append(r.queried, id)
	return slices.Clone(r.pids), r.err
}

func TestHostPIDsIncludeAttachmentsBeforeHostPIDRecorded(t *testing.T) {
	f := newFixture(t, fakeOwner{})
	_, live := f.running(t, "t1")
	_, asleep := f.running(t, "t2")
	_, preparing := f.running(t, "t3") // no saved launch: cannot have an attached host
	f.core.Store.DB.Exec(`UPDATE sessions SET spec = '{}' WHERE id = ?`, live)
	f.core.Store.DB.Exec(`UPDATE sessions SET host_pid = 4322, state = ? WHERE id = ?`, string(control.Asleep), asleep)
	f.core.Store.DB.Exec(`UPDATE sessions SET spec = '', host_pid = 0 WHERE id = ?`, preparing)
	r := &attachedRunner{Runner: f.core.Runner, pids: []int{9876}}
	f.core.Runner = r
	got, err := f.core.hostPIDs(context.Background())
	if err != nil || !slices.Equal(got, []int{9876}) || !slices.Equal(r.queried, []int64{int64(live)}) {
		t.Fatalf("pids %v, queried %v: %v", got, r.queried, err)
	}
	r.err = errors.New("older host cannot identify peers")
	if _, err := f.core.hostPIDs(context.Background()); err == nil {
		t.Fatal("unknown attachments accepted")
	}
}

type attachedInput struct {
	*fakeInput
	pid int
}

func (in *attachedInput) Check(ctx context.Context, pane string, pids []int, full bool) (*control.TmuxPane, bool, error) {
	p, own, err := in.fakeInput.Check(ctx, pane, pids, full)
	return p, own || full && slices.Contains(pids, in.pid), err
}

// The operation starts on an ordinary shell, then an attach appears. The
// next batch must refresh ownership even before the periodic check runs.
func TestHostInputRefreshesAttachedPIDsBeforeKeys(t *testing.T) {
	old := hostCheckEvery
	hostCheckEvery = time.Hour
	defer func() { hostCheckEvery = old }()
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "new attach", true: "unknown host"}[unknown], func(t *testing.T) {
			in := &fakeInput{}
			f := inputFixture(t, in)
			_, live := f.running(t, "t1")
			f.core.Store.DB.Exec(`UPDATE sessions SET host_pid = 4321 WHERE id = ?`, live)
			r := &attachedRunner{Runner: f.core.Runner}
			f.core.Runner = r
			f.core.HostInput = &attachedInput{fakeInput: in, pid: 9876}
			ctx := context.Background()
			op, err := f.core.As(operator).OperateHost(ctx, "%1")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { op.End(); <-op.Done() }()
			r.mu.Lock()
			if unknown {
				r.err = errors.New("cannot identify peers")
			} else {
				r.pids = []int{9876}
			}
			r.mu.Unlock()
			if err := op.Send(ctx, []control.HostKey{{Text: "must not reach the agent"}}); err == nil {
				t.Fatal("attachment accepted keys")
			}
			if in.sent() != 0 {
				t.Fatal("keys reached an attached terminal")
			}
		})
	}
}

func (r *fakeRunner) AttachedPIDs(context.Context, int64) ([]int, error) { return nil, nil }
