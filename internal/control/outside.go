package control

// OutsideText identifies text the daemon gave a conversation from outside
// the people allowed to act. IDs name messages, files, look-ups or turns;
// they never contain the text itself. Since protocol 3.43.
type OutsideText struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
