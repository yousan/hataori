package core

import (
	"context"
	"fmt"

	"github.com/yousan/hataori/internal/control"
)

// Remind is not the core's: only the daemon's supervisor client keeps
// reminders, on a supervisor's token connection (design, section 7.1).
func (a *api) Remind(ctx context.Context, r control.RemindRequest) (control.RemindAnswer, error) {
	a.c.Log.Warn("control call refused", "op", "Remind", "principal", string(a.caller.Principal), "from", a.caller.From,
		"reason", "only a supervisor's connection sets reminders")
	return control.RemindAnswer{}, fmt.Errorf("core: Remind: only a supervisor's connection sets reminders: %w", control.ErrNotSupported)
}
