package control

import "context"

// WorkerAttacher is an optional owner-only API. Keeping it separate preserves
// Attachment.Socket as a local Unix address for every existing client.
type WorkerAttacher interface {
	AttachWorker(context.Context, ThreadID) (WorkerAttachment, error)
}

// WorkerAttachment is configured connection information and one exact owned
// target. It contains no session/forge/bot token and grants no other scope.
type WorkerAttachment struct {
	Number       int64  `json:"number"`
	Destination  string `json:"destination"`
	Root         string `json:"root"`
	Main         string `json:"main"`
	Installation string `json:"installation"`
	Thread       uint64 `json:"thread"`
	Session      string `json:"session"`
	Run          string `json:"run"`
}
