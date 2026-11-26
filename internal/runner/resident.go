package runner

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// resident is whether the runner intends to keep this agent awake. A
// draining or failed agent is already being stopped. A window taken in
// from a person is not managed by this cap. lv.mu must be held.
func (lv *live) resident() bool {
	return !lv.asleep && !lv.failed && !lv.ended && lv.drain.IsZero() && lv.taken == nil
}

// checkResidentCap is called by the followers, outside their session
// locks. At most one pass runs per second, regardless of the session count.
func (r *Runner) checkResidentCap() {
	if r.MaxResidentAgents <= 0 || !r.residentMu.TryLock() {
		return
	}
	if time.Since(r.residentAt) < time.Second {
		r.residentMu.Unlock()
		return
	}
	r.residentAt = time.Now()
	// Looking at another session may wait for its follower or its host.
	// Keep those waits off the caller's event-reading loop. Close waits
	// for this pass before the store may be closed.
	go func() {
		defer r.residentMu.Unlock()
		r.enforceResidentCap()
	}()
}

func (r *Runner) enforceResidentCap() {
	r.mu.Lock()
	lives := make([]*live, 0, len(r.live))
	for _, lv := range r.live {
		lives = append(lives, lv)
	}
	r.mu.Unlock()
	if len(lives) <= r.MaxResidentAgents {
		return
	}
	count := func() int {
		n := 0
		for _, lv := range lives {
			lv.mu.Lock()
			if lv.resident() {
				n++
			}
			lv.mu.Unlock()
		}
		return n
	}
	if count() <= r.MaxResidentAgents {
		return
	}
	type candidate struct {
		live *live
		idle int64
	}
	var candidates []candidate
	ctx := context.Background()
	for _, lv := range lives {
		lv.mu.Lock()
		managed := lv.resident()
		lv.mu.Unlock()
		if !managed {
			continue
		}
		var state string
		var updated int64
		if err := r.Store.DB.QueryRowContext(ctx, `SELECT state, updated_at FROM sessions WHERE id = ?`, lv.id).Scan(&state, &updated); err != nil {
			r.Log.Error("resident cap", "session", lv.id, "err", err)
			return
		}
		if control.State(state) == control.Idle {
			candidates = append(candidates, candidate{live: lv, idle: updated})
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if c := cmp.Compare(a.idle, b.idle); c != 0 {
			return c
		}
		return cmp.Compare(a.live.id, b.live.id)
	})
	for _, c := range candidates {
		// Explicit sleep, idle timeout or exit may have reduced the count
		// since the snapshot. Do not put another agent to sleep for them.
		if count() <= r.MaxResidentAgents {
			return
		}
		c.live.mu.Lock()
		_, err := r.sleepIfIdleAt(ctx, c.live, c.idle)
		c.live.mu.Unlock()
		if err != nil {
			r.Log.Error("resident cap", "session", c.live.id, "err", err)
			return
		}
	}
}
