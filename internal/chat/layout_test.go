package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface/fake"
)

// layoutAPI records SetLayout's calls, failing the first fail of them
// with err.
type layoutAPI struct {
	control.API
	mu    sync.Mutex
	fail  int
	err   error
	calls int
}

func (a *layoutAPI) SetLayout(context.Context, string, []control.LayoutGroup) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls <= a.fail {
		return a.err
	}
	return nil
}

// A layout that could not be recorded is tried again, unless the core
// refused it; a burst of changes is recorded once.
func TestLayoutTriedAgain(t *testing.T) {
	defer func(d, r time.Duration) { layoutDelay, layoutRetry = d, r }(layoutDelay, layoutRetry)
	layoutDelay, layoutRetry = 20*time.Millisecond, 50*time.Millisecond
	for _, c := range []struct {
		name  string
		err   error
		calls int
	}{
		{"a failure", errors.New("the event could not be written"), 2},
		{"a refusal", fmt.Errorf("core: SetLayout: %w", control.ErrForbidden), 1},
	} {
		api := &layoutAPI{fail: 1, err: c.err}
		fe := &FrontEnd{Name: "chat", People: testPeople(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Driver: fake.New().WithLayout(),
			API:    func(control.Caller) control.API { return api }, Scopes: map[string]Scope{"c1": {Enabled: true}}}
		for range 3 {
			fe.LayoutChanged()
		}
		fe.bg.Wait()
		if api.calls != c.calls {
			t.Errorf("after %s: %d calls of SetLayout, want %d", c.name, api.calls, c.calls)
		}
	}
}
