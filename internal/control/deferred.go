package control

import "time"

const ViaDeferred = "deferred"

type DeferredSendRequest struct {
	Action string   `json:"action"` // queue, inspect, find, cancel
	Number int64    `json:"number,omitempty"`
	Target ThreadID `json:"target,omitempty"`
	Key    string   `json:"key,omitempty"`
	Text   string   `json:"text,omitempty"`
}

type DeferredSend struct {
	Number   int64     `json:"number"`
	State    string    `json:"state"`
	Target   ThreadID  `json:"target"`
	Session  int64     `json:"session"`
	Text     string    `json:"text,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Proposal int64     `json:"proposal,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}
