package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// LaunchConfig is what the loaded config gives a launch of a confined
// session (Runner.ConfinedConfig). It is stored with the session's spec, to
// be compared by a later daemon, so it holds no secret.
type LaunchConfig struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// Parts are the digests of what else the launch is given, such as its
	// instructions, by the name a log line calls each by.
	Parts map[string]string `json:"parts,omitempty"`
}

// Changed names what differs between c and old.
func (c LaunchConfig) Changed(old LaunchConfig) []string {
	var names []string
	if c.Model != old.Model {
		names = append(names, "model")
	}
	if c.Effort != old.Effort {
		names = append(names, "effort")
	}
	var parts []string
	for n, d := range c.Parts {
		if o, ok := old.Parts[n]; !ok || o != d {
			parts = append(parts, n)
		}
	}
	for n := range old.Parts {
		if _, ok := c.Parts[n]; !ok {
			parts = append(parts, n)
		}
	}
	slices.Sort(parts)
	return append(names, parts...)
}

// staleCheck is how often a stale session that reports nothing is tried to
// be put to sleep.
const staleCheck = time.Second

// compare marks lv, when its session is confined, as stale when its agent
// was launched with another config than a launch would be given now, and
// logs what changed.
func (r *Runner) compare(lv *live) error {
	if lv.spec.Confine == nil || r.ConfinedConfig == nil {
		return nil
	}
	ctx := context.Background()
	thread, scope, _, err := r.confinement(ctx, lv.id)
	if err != nil {
		return err
	}
	var specJSON string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT spec FROM sessions WHERE id = ?`, lv.id).Scan(&specJSON); err != nil {
		return fmt.Errorf("runner: session %d: %w", lv.id, err)
	}
	var s storedSpec
	if err := json.Unmarshal([]byte(specJSON), &s); err != nil {
		return fmt.Errorf("runner: session %d: spec: %w", lv.id, err)
	}
	now, ok := r.ConfinedConfig(scope, s.Agent)
	if !ok {
		return nil // nothing would launch it again
	}
	// An older hataori recorded nothing: what it gave is not known.
	changed := "not recorded"
	if s.Config != nil {
		names := now.Changed(*s.Config)
		if len(names) == 0 {
			return nil
		}
		changed = strings.Join(names, ", ")
	}
	lv.stale = true
	r.Log.Info("the session was launched with another config: it is put to sleep at its first idle moment, and its next message launches it with this one",
		"session", lv.id, "thread", int64(thread), "scope", scope, "changed", changed)
	return nil
}

// checkStale puts lv to sleep (Sleep) when it is stale and idle: when its
// agent reported something, and every staleCheck. lv.mu is held.
func (r *Runner) checkStale(lv *live, reported bool) {
	if !lv.stale || !lv.drain.IsZero() || !reported && time.Now().Before(lv.staleAt) {
		return
	}
	lv.staleAt = time.Now().Add(staleCheck)
	ctx := context.Background()
	// Before the host is asked for attached terminals: it is busy most of
	// the time it is tried. A first input its agent was given and did not
	// report yet is not left behind.
	var state, first string
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT state, first_input FROM sessions WHERE id = ?`, lv.id).Scan(&state, &first); err != nil ||
		control.State(state) != control.Idle || first != "" {
		return
	}
	if _, err := r.sleep(ctx, lv); err != nil {
		r.Log.Error("putting a session launched with another config to sleep", "session", lv.id, "err", err)
	}
}

// settleTime is how long Reconnect waits for the stale sessions at most.
const settleTime = 5 * time.Second

// settle waits, at most settleTime, until each session found launched with
// another config has been tried to be put to sleep once, so that a message
// that comes after it does not start a turn in a session that was idle all
// the while.
func (r *Runner) settle(ctx context.Context) {
	r.mu.Lock()
	var lives []*live
	for _, lv := range r.live {
		if lv.stale {
			lives = append(lives, lv)
		}
	}
	r.mu.Unlock()
	end := time.Now().Add(settleTime)
	for _, lv := range lives {
		// Held from follow until the loop's first step is done.
		for !lv.mu.TryLock() {
			if ctx.Err() != nil || time.Now().After(end) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		lv.mu.Unlock()
	}
}
