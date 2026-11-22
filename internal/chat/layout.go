package chat

import (
	"context"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

var _ surface.LayoutWatcher = (*FrontEnd)(nil)

// layoutDelay is how long a layout change waits before it is recorded, so
// that a burst of them, such as the channels and threads of each server
// right after a connect, is recorded once. A variable for tests.
var layoutDelay = 300 * time.Millisecond

// layoutState is whether a layout is being recorded, and whether a change
// came since that one was read.
type layoutState struct {
	mu      sync.Mutex
	running bool
	pending bool
}

// LayoutChanged records the driver's layout in the core (SetLayout), when
// the driver can tell one (surface.Layouter): Connected calls it, and the
// driver calls it after each change. Changes that come while one is
// recorded are recorded once more after it, newest last.
func (f *FrontEnd) LayoutChanged() {
	if _, ok := f.Driver.(surface.Layouter); !ok {
		return
	}
	l := &f.layout
	l.mu.Lock()
	l.pending = true
	if l.running {
		l.mu.Unlock()
		return
	}
	l.running = true
	l.mu.Unlock()
	f.bg.Add(1)
	go f.recordLayout(f.context())
}

// recordLayout reads the driver's layout and records it, until no change
// is pending.
func (f *FrontEnd) recordLayout(ctx context.Context) {
	defer f.bg.Done()
	l := &f.layout
	for {
		select {
		case <-ctx.Done():
		case <-time.After(layoutDelay):
		}
		l.mu.Lock()
		if !l.pending || ctx.Err() != nil {
			l.running, l.pending = false, false
			l.mu.Unlock()
			return
		}
		l.pending = false
		l.mu.Unlock()
		lay, err := f.Driver.(surface.Layouter).Layout(ctx)
		if err != nil {
			f.Log.Warn("chat: the chat's layout could not be read", "err", err)
			continue
		}
		api := f.API(control.Caller{Principal: f.ownerPrincipal(), Scopes: f.scopeList(), May: control.OpName})
		if err := api.SetLayout(ctx, f.Name, layoutGroups(lay)); err != nil && ctx.Err() == nil {
			f.Log.Warn("chat: the chat's layout could not be recorded", "err", err)
		}
	}
}

// layoutGroups is a driver's layout in the control API's terms.
func layoutGroups(l surface.Layout) []control.LayoutGroup {
	out := make([]control.LayoutGroup, 0, len(l.Groups))
	for _, g := range l.Groups {
		cg := control.LayoutGroup{ID: g.ID, Name: g.Name, Channels: make([]control.LayoutChannel, 0, len(g.Channels))}
		for _, c := range g.Channels {
			cc := control.LayoutChannel{ID: c.ID, Name: c.Name, URL: c.URL, Threads: make([]control.LayoutThread, 0, len(c.Threads))}
			for _, t := range c.Threads {
				cc.Threads = append(cc.Threads, control.LayoutThread{ID: t.ID, Name: t.Name, URL: t.URL})
			}
			cg.Channels = append(cg.Channels, cc)
		}
		out = append(out, cg)
	}
	return out
}
