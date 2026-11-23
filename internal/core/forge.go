package core

import (
	"context"
	"fmt"

	"github.com/yousan/hataori/internal/control"
)

// Forge is not the core's: only the daemon's supervisor client reads a
// forge, on a supervisor's token connection (design, section 7.1).
func (a *api) Forge(ctx context.Context, r control.ForgeRequest) (control.ForgeAnswer, error) {
	a.c.Log.Warn("control call refused", "op", "Forge", "principal", string(a.caller.Principal), "from", a.caller.From,
		"reason", "only a supervisor's connection reads a forge")
	return control.ForgeAnswer{}, fmt.Errorf("core: Forge: only a supervisor's connection reads a forge: %w", control.ErrNotSupported)
}
