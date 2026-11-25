package core

import (
	"context"
	"fmt"

	"github.com/yousan/hataori/internal/control"
)

// Files is not the core's: only the daemon's supervisor client keeps a
// supervisor's directory, on a supervisor's token connection (design,
// section 7.1).
func (a *api) Files(ctx context.Context, r control.FilesRequest) (control.FilesAnswer, error) {
	a.c.Log.Warn("control call refused", "op", "Files", "principal", string(a.caller.Principal), "from", a.caller.From,
		"reason", "only a supervisor's connection reads and writes its files")
	return control.FilesAnswer{}, fmt.Errorf("core: Files: only a supervisor's connection reads and writes its files: %w", control.ErrNotSupported)
}
