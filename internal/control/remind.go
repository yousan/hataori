package control

import "time"

// RemindRequest sets a reminder, a wake of the scope's supervisor at a time
// it asks for, or cancels an open one (API.Remind). Exactly one of At,
// Delay and Cancel is given.
type RemindRequest struct {
	At     *time.Time `json:"at,omitempty"`     // when, from 5 minutes to 7 days ahead
	Delay  int64      `json:"delay,omitempty"`  // or after this many seconds, as far ahead
	Note   string     `json:"note,omitempty"`   // what the wake gives back, at most 1024 bytes; needed to set one
	Cancel int64      `json:"cancel,omitempty"` // the number of the open reminder to cancel
}

// RemindAnswer is the reminder that was set or cancelled.
type RemindAnswer struct {
	Number int64     `json:"number"`
	At     time.Time `json:"at"`
}
