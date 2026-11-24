package plugin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/people"
)

// may is what a handler at a point may do besides letting it pass as it
// is: stop it (a gate), change its text, or add text before and after it
// (modify).
type may struct{ stop, change, add bool }

var kinds = map[string]may{
	"before_agent_run":    {stop: true},
	"before_prompt_build": {add: true},
	"message_sending":     {stop: true, change: true},
}

// Conn is one connection of a plugin on which the daemon sends requests:
// a control.Peer.
type Conn interface {
	Request(ctx context.Context, op string, args, result any) error
	Done() <-chan struct{}
}

// Dispatcher runs the plugins' hook handlers (design, section 7.3). It
// knows each plugin's connections from control.Server.Peered, by the API
// that BindAs marked, and calls a handler on the plugin's oldest
// connection that is still open.
type Dispatcher struct {
	grants []Grant
	log    *slog.Logger

	mu    sync.Mutex
	conns map[string][]Conn // by plugin id, oldest first
}

var _ control.Hooks = (*Dispatcher)(nil)

// NewDispatcher returns the dispatcher of grants, in the config's order.
func NewDispatcher(grants []Grant, log *slog.Logger) *Dispatcher {
	return &Dispatcher{grants: grants, log: log, conns: map[string][]Conn{}}
}

// bound is an API the core bound to a plugin's caller, marked with the
// plugin's id.
type bound struct {
	control.API
	id string
}

// BindAs returns as, which binds a caller to the core's API, marking the
// API of a plugin's caller (From "plugin:<id>", which only Grant.Caller
// makes) so that the dispatcher knows the connections made with its
// token.
func BindAs(as func(control.Caller) control.API) func(control.Caller) control.API {
	return func(c control.Caller) control.API {
		api := as(c)
		if client, id, ok := people.Split(c.From); ok && client == Client && c.Limit != nil {
			return bound{API: api, id: id}
		}
		return api
	}
}

// Peered takes a connection that control.Server says came up. One that is not
// a plugin's is ignored. A plugin that had no connection open is told
// plugin_started on it, with its grant.
func (d *Dispatcher) Peered(api control.API, c Conn) {
	b, ok := api.(bound)
	if !ok {
		return
	}
	g, ok := d.grant(b.id)
	if !ok {
		return
	}
	d.mu.Lock()
	first := len(d.live(g.ID)) == 0
	d.conns[g.ID] = append(d.conns[g.ID], c)
	d.mu.Unlock()
	go func() {
		<-c.Done()
		d.mu.Lock()
		defer d.mu.Unlock()
		d.conns[g.ID] = slices.DeleteFunc(d.conns[g.ID], func(o Conn) bool { return o == c })
	}()
	if first {
		go d.started(g, c)
	}
}

func (d *Dispatcher) grant(id string) (Grant, bool) {
	for _, g := range d.grants {
		if g.ID == id {
			return g, true
		}
	}
	return Grant{}, false
}

// live is plugin id's connections that are open, oldest first; d.mu is
// held.
func (d *Dispatcher) live(id string) []Conn {
	var out []Conn
	for _, c := range d.conns[id] {
		select {
		case <-c.Done():
		default:
			out = append(out, c)
		}
	}
	return out
}

// conn is the connection plugin id's handlers are called on, or nil.
func (d *Dispatcher) conn(id string) Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l := d.live(id); len(l) > 0 {
		return l[0]
	}
	return nil
}

func (d *Dispatcher) started(g Grant, c Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	s := control.PluginStarted{Plugin: g.ID, Scopes: g.Scopes, Operations: g.Operations, Conversation: g.Conversation}
	for _, k := range g.Events {
		s.Events = append(s.Events, string(k))
	}
	for _, h := range g.Hooks {
		s.Hooks = append(s.Hooks, control.GrantedHook{Point: h.Point, Priority: h.Priority,
			Timeout: h.Timeout.Milliseconds(), OnFailure: string(policy(h))})
	}
	if err := c.Request(ctx, control.OpPluginStarted, s, nil); err != nil {
		d.log.Info("plugin: plugin_started not taken", "plugin", g.ID, "why", failure(ctx, c, err))
	}
}

// Stopping tells plugin g that it is about to be stopped, and waits for
// its answer until ctx ends. A plugin with no connection open is told
// nothing. It is what plugin.Runner calls before it stops a plugin.
func (d *Dispatcher) Stopping(ctx context.Context, g Grant) {
	c := d.conn(g.ID)
	if c == nil {
		return
	}
	if err := c.Request(ctx, control.OpPluginStopping, control.PluginStopping{Plugin: g.ID, Left: left(ctx)}, nil); err != nil {
		d.log.Info("plugin: plugin_stopping not taken", "plugin", g.ID, "why", failure(ctx, c, err))
	}
}

// left is how long ctx has, in milliseconds; 0 when it has no deadline.
func left(ctx context.Context) int64 {
	if t, ok := ctx.Deadline(); ok {
		return max(time.Until(t).Milliseconds(), 0)
	}
	return 0
}

// policy is what h's failure does: the config's on_failure, else its
// point's, else block.
func policy(h Hook) Failure {
	if h.OnFailure != "" {
		return h.OnFailure
	}
	if f, ok := points[h.Point]; ok {
		return f
	}
	return Block
}

type handler struct {
	g Grant
	h Hook
}

// Run runs the handlers on c.Point of the plugins whose grant has the
// point and c.Scope: by priority, higher first, then in the config's
// order, each given the text as the one before left it. A handler fails
// when it does not answer within its timeout, when its plugin is not
// connected or its connection closes, when it answers with an error
// (not_supported too), and when its answer is one its point does not
// allow. A failure blocks or skips by the handler's policy. Each call is
// logged, without text.
func (d *Dispatcher) Run(ctx context.Context, c control.HookCall) control.HookResult {
	var hs []handler
	for _, g := range d.grants {
		if !slices.Contains(g.Scopes, c.Scope) {
			continue
		}
		for _, h := range g.Hooks {
			if h.Point == c.Point {
				hs = append(hs, handler{g, h})
			}
		}
	}
	slices.SortStableFunc(hs, func(a, b handler) int { return cmp.Compare(b.h.Priority, a.h.Priority) })
	res := control.HookResult{Text: c.Text}
	for _, x := range hs {
		began := time.Now()
		// Each handler sees what the ones before it left: with the text
		// they added, around it.
		a, why := d.call(ctx, x, c, control.Input{Before: res.Before, Text: res.Text, After: res.After}.ForAgent())
		log := []any{"plugin", x.g.ID, "point", c.Point, "thread", c.Thread, "scope", c.Scope, "took", time.Since(began)}
		if why == "" {
			why = allowed(c, a)
		}
		if why != "" {
			p := policy(x.h)
			d.log.Warn("plugin hook failed", append(log, "why", why, "policy", string(p))...)
			if p == Block {
				return control.HookResult{Text: res.Text, Stopped: true, Failed: true, By: x.g.ID, Reason: why}
			}
			continue
		}
		if a.Action == control.HookStop {
			d.log.Info("plugin hook", append(log, "outcome", "stop")...)
			return control.HookResult{Text: res.Text, Stopped: true, By: x.g.ID, Reason: a.Reason}
		}
		outcome := "continue"
		if a.Text != nil {
			res.Text, outcome = *a.Text, "changed"
		}
		if a.Before != "" || a.After != "" {
			res.Before = control.Input{Before: a.Before, Text: res.Before}.ForAgent()
			res.After = control.Input{Text: res.After, After: a.After}.ForAgent()
			outcome = "added"
		}
		d.log.Info("plugin hook", append(log, "outcome", outcome)...)
	}
	return res
}

// call calls one handler, and returns its answer, or why it failed.
func (d *Dispatcher) call(ctx context.Context, x handler, c control.HookCall, text string) (control.HookAnswer, string) {
	var a control.HookAnswer
	if ctx.Err() != nil {
		// Nothing is sent: a write with a context that has ended closes
		// the plugin's connection (wire.Peer).
		return a, "the call was given up"
	}
	conn := d.conn(x.g.ID)
	if conn == nil {
		return a, "it is not connected"
	}
	hctx, cancel := context.WithTimeout(ctx, x.h.Timeout)
	defer cancel()
	req := control.HookRequest{Point: c.Point, Thread: c.Thread, Scope: c.Scope, Left: left(hctx)}
	if x.g.Conversation {
		req.Text = text
	}
	if err := conn.Request(hctx, control.OpHook, req, &a); err != nil {
		if ctx.Err() == nil && errors.Is(hctx.Err(), context.DeadlineExceeded) {
			return a, fmt.Sprintf("it did not answer within %v", x.h.Timeout)
		}
		return a, failure(ctx, conn, err)
	}
	return a, ""
}

// allowed says why answer a is not one that the point of c allows, or
// nothing. A post of no text and no file is none: a handler that wants
// nothing posted stops it.
func allowed(c control.HookCall, a control.HookAnswer) string {
	point := c.Point
	k := kinds[point]
	switch {
	case a.Action != control.HookStop && a.Action != control.HookContinue:
		return fmt.Sprintf("its answer's action is neither %s nor %s", control.HookContinue, control.HookStop)
	case a.Action == control.HookStop && !k.stop:
		return fmt.Sprintf("it answered stop, which %s does not allow", point)
	case a.Action == control.HookStop && a.Text != nil:
		return "it answered stop with a text"
	case a.Action == control.HookContinue && a.Text != nil && !k.change:
		return fmt.Sprintf("it answered a changed text, which %s does not allow", point)
	case a.Text != nil && strings.TrimSpace(*a.Text) == "" && !c.Files:
		return fmt.Sprintf("it answered a text with nothing in it, which %s does not allow", point)
	case (a.Before != "" || a.After != "") && a.Action == control.HookStop:
		return "it answered stop with text to add"
	case (a.Before != "" || a.After != "") && !k.add:
		return fmt.Sprintf("it answered text to add, which %s does not allow", point)
	}
	return ""
}

// failure says why a request on conn failed with err, in hataori's words
// and with no text of the plugin's; ctx is the request's.
func failure(ctx context.Context, conn Conn, err error) string {
	if ctx.Err() != nil {
		return "the call was given up"
	}
	select {
	case <-conn.Done():
		return "its connection closed"
	default:
	}
	var coded interface{ Code() string }
	code := ""
	if errors.As(err, &coded) {
		code = coded.Code()
	}
	switch {
	case code == "not_supported":
		return "it does not support the request (not_supported)"
	case code != "":
		return fmt.Sprintf("it answered with the error code %s", code)
	}
	return "the request could not be sent"
}
