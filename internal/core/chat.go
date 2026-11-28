package core

import (
	"context"
	"github.com/yousan/hataori/internal/control"
)

// ReadChat is answered only by the supervisor's token-bound client.
// The owner's socket, plugins and chat callers cannot use it through core.
func (a *api) ReadChat(context.Context, control.ReadChatRequest) (control.ReadChatAnswer, error) {
	a.c.Log.Warn("control call refused", "op", "ReadChat", "principal", string(a.caller.Principal), "from", a.caller.From,
		"reason", "only a supervisor's token connection reads service history")
	return control.ReadChatAnswer{}, control.ErrNotSupported
}
