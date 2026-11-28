package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// These fixtures contain records only: no host, agent, real PID or tmux.
func earlierEndRecord(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal(host.Status{Exited: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, host.StatusFile), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEndEarlierCompletedStopAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := earlierEndRecord(t)
		stopped := make(chan struct{})
		r := &Runner{endWait: time.Second, ending: map[int64]chan struct{}{1: stopped}}
		deadlineSelected := false
		r.point = func(point string) {
			if point == "earlier stop deadline" {
				deadlineSelected = true
				// Force the deadline branch while the stop is outstanding,
				// then finish it before the final decision. This exercises
				// that branch deterministically, including the recheck
				// needed when select chooses an already-ready deadline.
				close(stopped)
			}
		}
		if err := r.endEarlier(context.Background(), 1, dir); err != nil {
			t.Fatal("confirmed end and completed stop refused:", err)
		}
		if !deadlineSelected {
			t.Fatal("did not exercise the deadline branch")
		}
	})
}

func TestEndEarlierOutstandingStopBlocksReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := earlierEndRecord(t)
		stopped := make(chan struct{})
		r := &Runner{endWait: time.Second, ending: map[int64]chan struct{}{1: stopped}}
		// Model an earlier stop paused before it reaches the directory.
		// Its completion channel is the same authority barrier used by a
		// real stopEarlier, even after the host records a positive end.
		current := "earlier"
		release := make(chan struct{})
		reached := make(chan string, 1)
		if r.stopEarlier(1, dir) != stopped {
			t.Fatal("positive host end discarded the outstanding stop")
		}
		go func() {
			<-release
			reached <- current
			close(stopped)
		}()
		err := r.endEarlier(context.Background(), 1, dir)
		if err == nil {
			current = "replacement"
		}
		close(release)
		target := <-reached
		<-stopped
		if err == nil || !strings.Contains(err.Error(), "stopping its earlier agent has not ended") {
			t.Fatal("outstanding stop admitted replacement:", err)
		}
		if target != "earlier" {
			t.Fatalf("old stop reached %s", target)
		}
		if err := r.endEarlier(context.Background(), 1, dir); err != nil {
			t.Fatal("replacement refused after stop completion:", err)
		}
		current = "replacement"
	})
}

func TestEndEarlierNeedsPositiveHostEnd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state holder.State
	}{{"unknown", holder.Unknown}, {"running", holder.Running}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				if tc.state == holder.Running {
					dir = earlierEndRecord(t)
					// A recorded status with an unreleased socket is still
					// running. Pin every record inside this test's directory;
					// no socket is opened and no process is inspected.
					sock := filepath.Join(dir, "socket")
					for name, value := range map[string]string{
						"host.sock.path": sock,
						"socket":         "record only",
						host.HostPIDFile: "record only",
					} {
						if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				stopped := make(chan struct{})
				close(stopped)
				r := &Runner{endWait: time.Second, Poll: time.Millisecond, ending: map[int64]chan struct{}{1: stopped}}
				if got := r.alive(dir).State; got != tc.state {
					t.Fatalf("fixture state %v, want %v", got, tc.state)
				}
				if err := r.endEarlier(context.Background(), 1, dir); err == nil || !strings.Contains(err.Error(), "its host is still running") {
					t.Fatal("unconfirmed host admitted replacement:", err)
				}
			})
		})
	}
}

func TestEndEarlierCanceledWithOutstandingStop(t *testing.T) {
	dir := earlierEndRecord(t)
	stopped := make(chan struct{})
	r := &Runner{ending: map[int64]chan struct{}{1: stopped}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.endEarlier(ctx, 1, dir); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled wait admitted replacement:", err)
	}
}
