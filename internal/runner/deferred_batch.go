package runner

import (
	"strings"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/core"
)

// runReadActions finishes ordinary batch progress before a deferred handoff.
// False requires a fresh read/reconstruction before any first/pending input.
func (r *Runner) runReadActions(lv *live, actions []agent.Actions) bool {
	lv.deferredProgress = actionsHaveIO(actions)
	defer func() { lv.deferredProgress = false }()
	var progressErr error
	for i, a := range actions {
		if deferredAction(a) && progressErr != nil {
			for _, id := range a.Inputs {
				if strings.HasPrefix(id, core.DeferredKeyPrefix) {
					r.failDeferred(lv, id, "ordinary native progress failed before the deferred input could freeze")
				}
			}
			lv.warmPending, lv.pending = true, true
			continue
		}
		if deferredAction(a) && actionsHaveIO(actions[i+1:]) {
			// Do not freeze around ordinary responses already consumed from
			// this batch. Execute them, then reconstruct the still-unsent input.
			lv.warmPending, lv.pending = true, true
			continue
		}
		if err := r.act(lv, a); err != nil {
			progressErr = err
		}
		if lv.deferredBlocked {
			r.holdDeferredActions(lv, actions[i+1:])
			return false
		}
	}
	// The pre-read probe proved no uncertain receiver write could fence this
	// batch. Finish its ordinary responses before any absent first attempt;
	// deferredProgress requires a subsequent fresh read for that attempt.
	if err := r.reconcileDeferred(lv); err != nil {
		r.holdDeferredActions(lv, nil)
		r.Log.Warn("deferred input recovery waits", "session", lv.id, "err", err)
		return false
	}
	if lv.deferredBlocked {
		r.holdDeferredActions(lv, nil)
		return false
	}
	if lv.warmPending {
		return false // ordinary batch I/O is complete; never rehand into the old codec
	}
	return true
}
