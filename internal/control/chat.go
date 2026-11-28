package control

// ReadChatRequest reads service history, not an internal session's output.
// It is available only on a supervisor's token connection. Since 3.49.
type ReadChatRequest struct {
	Op string `json:"op"` // list or read
	// Channel is <client>:<channel>; empty selects the scope's own channel.
	Channel  string `json:"channel,omitempty"`
	Thread   string `json:"thread,omitempty"`   // service thread id, required for read
	Archived bool   `json:"archived,omitempty"` // list archived instead of active threads
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"` // read only; exclusive with before
	Limit    int    `json:"limit,omitempty"` // 1..50; default 50
}

// ReadChatAnswer contains already-quoted, trust-filtered text. Pages have
// at most 50 entries and 16 KiB of text. Files are named, never fetched.
type ReadChatAnswer struct {
	Text   string `json:"text,omitempty"`
	Next   string `json:"next,omitempty"`   // list continuation
	Before string `json:"before,omitempty"` // read continuation toward older messages
	After  string `json:"after,omitempty"`  // read continuation toward newer messages
}
