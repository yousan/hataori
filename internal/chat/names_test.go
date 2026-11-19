package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// nameAPI records SetName's calls, refusing the first fail of them as the
// core does while a thread is not linked yet.
type nameAPI struct {
	control.API
	mu     sync.Mutex
	fail   int
	calls  []string
	stored string
}

func (a *nameAPI) SetName(_ context.Context, _ control.LinkRef, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, name)
	if len(a.calls) <= a.fail {
		return errors.New("the conversation is linked to no thread yet")
	}
	a.stored = name
	return nil
}

// A thread's name refused at first is tried again, but not over a newer
// name that came meanwhile: the newest name is the one kept.
func TestRenamesKeepTheNewest(t *testing.T) {
	defer func(d time.Duration) { renameRetry = d }(renameRetry)
	renameRetry = 300 * time.Millisecond
	api := &nameAPI{fail: 1}
	fe := &FrontEnd{Name: "chat", People: testPeople(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		API: func(control.Caller) control.API { return api }, Scopes: map[string]Scope{"c1": {Enabled: true}}}
	conv := surface.Conversation{Channel: "c1", Thread: "t1"}
	fe.Renamed(conv, "old") // refused: not linked yet
	time.Sleep(100 * time.Millisecond)
	fe.Renamed(conv, "new") // while "old" waits to be tried again
	fe.bg.Wait()
	if !slices.Equal(api.calls, []string{"old", "new"}) || api.stored != "new" {
		t.Fatalf("calls %q, stored %q; want the newest name stored last", api.calls, api.stored)
	}

	// Refused twice, a name is given up; the next one is recorded.
	api = &nameAPI{fail: 2}
	fe.Renamed(conv, "once")
	fe.bg.Wait()
	fe.Renamed(conv, "twice")
	fe.bg.Wait()
	if !slices.Equal(api.calls, []string{"once", "once", "twice"}) || api.stored != "twice" {
		t.Fatalf("calls %q, stored %q", api.calls, api.stored)
	}
}
