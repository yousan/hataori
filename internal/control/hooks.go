package control

import (
	"context"
	"errors"
	"fmt"
)

// Hooks runs the handlers that plugins have on a hook point (design,
// section 7.3). The core and the chat front end call it at their points,
// and so name no plugin. A point with no handler calls nothing and waits
// for nothing.
type Hooks interface {
	Run(ctx context.Context, c HookCall) HookResult
}

// HookCall is what passes one hook point.
type HookCall struct {
	Point  string   // such as before_agent_run
	Thread ThreadID // 0 at a point before the thread has a number
	Scope  string
	Text   string // the input, or the post
	// Files says that the post has files, so that it is something to post
	// without a text too.
	Files bool
}

// HookResult is what comes out of a hook point.
type HookResult struct {
	// Text is what passes the point: HookCall.Text, as the handlers
	// changed it one after another.
	Text string
	// Before and After are the text the handlers added before and after
	// it, at a point that allows that (before_prompt_build): each
	// handler's around what the ones before it left, on lines of their
	// own.
	Before, After string
	// Stopped is whether it does not pass: a handler stopped it, or failed
	// where its policy blocks. By names that handler's plugin, and Reason
	// says why: the plugin's own reason, which is text, or, when Failed,
	// hataori's account of the failure, which is not.
	Stopped bool
	Failed  bool
	By      string
	Reason  string
}

// The requests the daemon sends a client, by their names on the wire.
// Since 3.16.
const (
	// OpHook calls one handler of a plugin at a hook point (design,
	// section 7.3).
	OpHook = "hook"
	// OpPluginStarted tells a plugin that it is connected, with its grant.
	OpPluginStarted = "plugin_started"
	// OpPluginStopping tells a plugin that it is about to be stopped.
	OpPluginStopping = "plugin_stopping"
)

// HookRequest is the arguments of OpHook.
type HookRequest struct {
	Point string `json:"point"`
	// Thread is the thread's number; 0, and absent, at a point before the
	// thread has one.
	Thread ThreadID `json:"thread,omitempty"`
	Scope  string   `json:"scope"`
	// Text is what passes the point: the input, or the post. It is text,
	// left out for a plugin without the "conversation" grant.
	Text string `json:"text,omitempty"`
	// Left is how long the handler has to answer, in milliseconds.
	Left int64 `json:"left"`
}

// The actions of a HookAnswer.
const (
	HookContinue = "continue"
	HookStop     = "stop"
)

// HookAnswer is the result of OpHook.
type HookAnswer struct {
	Action string `json:"action"` // HookContinue or HookStop
	// Text, with HookContinue, is what passes the point instead; absent,
	// it passes unchanged.
	Text *string `json:"text,omitempty"`
	// Reason, with HookStop, is why, which the thread shows; text.
	Reason string `json:"reason,omitempty"`
	// Before and After, with HookContinue, are text to add before and
	// after what passes the point, at a point that allows that. Since
	// 3.18.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// PluginStarted is the arguments of OpPluginStarted: the plugin's grant,
// what its manifest asks for and the config grants.
type PluginStarted struct {
	Plugin       string        `json:"plugin"`
	Scopes       []string      `json:"scopes"`
	Operations   []string      `json:"operations,omitempty"`
	Events       []string      `json:"events,omitempty"`
	Hooks        []GrantedHook `json:"hooks,omitempty"`
	Conversation bool          `json:"conversation,omitempty"`
}

// GrantedHook is one hook point of a plugin's grant.
type GrantedHook struct {
	Point     string `json:"point"`
	Priority  int    `json:"priority,omitempty"`
	Timeout   int64  `json:"timeout"`    // milliseconds
	OnFailure string `json:"on_failure"` // "block" or "skip"
}

// PluginStopping is the arguments of OpPluginStopping.
type PluginStopping struct {
	Plugin string `json:"plugin"`
	// Left is how long the plugin has before it is stopped, in
	// milliseconds.
	Left int64 `json:"left"`
}

// ErrStopped is returned by Start and Send when a plugin's handler on an
// input hook point stopped the input, or failed where its policy blocks:
// nothing was accepted. The error is a *Stopped. Since 3.18.
var ErrStopped = errors.New("stopped by a plugin")

// Stopped says which plugin stopped an input, at which point, and why.
type Stopped struct {
	Point  string
	Plugin string
	// Reason is the plugin's own, which is text, or, when Failed,
	// hataori's account of the plugin's failure.
	Reason string
	Failed bool
}

func (s *Stopped) Error() string {
	if s.Failed {
		return fmt.Sprintf("the plugin %s failed at %s, and its policy blocks the input: %s", s.Plugin, s.Point, s.Reason)
	}
	return fmt.Sprintf("the plugin %s stopped the input at %s: %s", s.Plugin, s.Point, s.Reason)
}

func (s *Stopped) Is(target error) bool { return target == ErrStopped }
