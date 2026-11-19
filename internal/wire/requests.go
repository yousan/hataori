package wire

import "github.com/yousan/hataori/internal/control"

// requests is every request the daemon may send a client: the type of its
// arguments and of its answer, nil for none. docs/protocol.md describes
// each, and a test keeps the two equal.
var requests = map[string]struct{ args, result any }{
	control.OpHook:           {control.HookRequest{}, control.HookAnswer{}},
	control.OpPluginStarted:  {control.PluginStarted{}, nil},
	control.OpPluginStopping: {control.PluginStopping{}, nil},
}

// Code is the error code on the wire the other side answered with, when
// it is one of the protocol's; "error" for any other, so that a code the
// other side made up, which may be any text, goes no further.
func (e *remoteError) Code() string {
	for _, c := range codes {
		if c.code == e.code {
			return e.code
		}
	}
	return "error"
}
