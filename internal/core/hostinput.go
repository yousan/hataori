package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// HostInput types into tmux panes on the host that hataori does not own
// (design, section 7.2); hostterm.Lister is one.
type HostInput interface {
	// Check looks at a pane as tmux lists it now: nil when it is not
	// listed; own when one of hataori's processes runs there, by the
	// command tmux reports and, with procs, by the process table, where a
	// process under one of owned counts as hataori's.
	Check(ctx context.Context, pane string, owned []int, procs bool) (p *control.TmuxPane, own bool, err error)
	// Keys types keys into pane, text through buffer, which it deletes
	// after each paste and after a failure.
	Keys(ctx context.Context, pane, buffer string, keys []control.HostKey) error
	// NewBuffer names a tmux buffer for one operation.
	NewBuffer() (string, error)
	// DropBuffer deletes buffer if tmux has it.
	DropBuffer(ctx context.Context, buffer string) error
}

// DefaultHostInputIdle is how long an operation lasts without a key when
// the config does not say.
const DefaultHostInputIdle = 5 * time.Minute

// hostCheckEvery is how often an operation reads the process table and
// the pane's command between batches.
var hostCheckEvery = time.Second

// hostCmdTimeout bounds each step that runs tmux or ps for an operation.
const hostCmdTimeout = 10 * time.Second

// hostOpSet is the panes being operated, by pane id.
type hostOpSet struct {
	mu      sync.Mutex
	byPane  map[string]*hostOp
	running sync.WaitGroup // until each has finished
	stopped bool           // StopHostInput was called: no operation starts
}

// hostOp is one pane being operated: a control.HostOperation.
type hostOp struct {
	c      *Core
	pane   control.TmuxPane // as it was at the start
	person string
	since  time.Time
	buffer string
	idle   time.Duration

	sendMu sync.Mutex // one batch at a time; the end waits for it

	// live ends when the operation does, which stops a batch being sent
	// and a check being made.
	live     context.Context
	stopLive context.CancelFunc
	checking atomic.Bool    // a check runs now
	checks   sync.WaitGroup // the checks running

	mu      sync.Mutex
	reason  string    // set once, by end
	lastKey time.Time // the last batch with a key, or the start
	stop    chan struct{}
	done    chan struct{}
}

// paneBusy is the error of an OperateHost of a pane being operated.
type paneBusy struct {
	pane, person string
	since        time.Time
}

func (e *paneBusy) Error() string {
	return fmt.Sprintf("core: OperateHost: tmux pane %s is being operated by %s since %s", e.pane, e.person, e.since.Format(time.RFC3339))
}

func (e *paneBusy) Is(target error) bool { return target == ErrBusy }

// checkHostInput allows OperateHost: only when host input is on, and only
// to the owner's caller with OpHostInput. Every refusal is one log line.
func (a *api) checkHostInput() error {
	if err := a.granted("OperateHost", ""); err != nil {
		return err
	}
	reason := ""
	switch {
	case a.c.HostInput == nil || a.c.HostTerms == nil:
		reason = "host input is off"
	case a.c.People == nil || a.c.People.Owner() == "" || a.caller.Principal != control.Principal(a.c.People.Owner()):
		reason = "not the owner"
	case !a.caller.May.Has(control.OpHostInput):
		reason = "the caller may not type into host panes"
	default:
		return nil
	}
	a.c.Log.Warn("control call refused", "op", "OperateHost", "principal", string(a.caller.Principal), "from", a.caller.From, "reason", reason)
	return fmt.Errorf("core: OperateHost: %s: %w", reason, ErrForbidden)
}

// stopping reports whether the daemon is stopping.
func (c *Core) stopping() bool {
	select {
	case <-c.Stopping:
		return true
	default:
		return false
	}
}

func (a *api) OperateHost(ctx context.Context, pane string) (control.HostOperation, error) {
	if err := a.checkHostInput(); err != nil {
		return nil, err
	}
	c := a.c
	if c.stopping() {
		return nil, errors.New("core: OperateHost: the daemon is stopping")
	}
	owned, err := c.hostPIDs(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, hostCmdTimeout)
	p, own, err := c.HostInput.Check(cctx, pane, owned, true)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("core: OperateHost: %w", err)
	}
	if p == nil {
		return nil, fmt.Errorf("core: OperateHost: tmux pane %q: %w", pane, ErrNotFound)
	}
	if own {
		c.Log.Warn("control call refused", "op", "OperateHost", "principal", string(a.caller.Principal), "pane", p.Pane,
			"reason", "one of hataori's processes runs in the pane")
		return nil, fmt.Errorf("core: OperateHost: tmux pane %s runs one of hataori's processes: %w", p.Pane, ErrForbidden)
	}
	buffer, err := c.HostInput.NewBuffer()
	if err != nil {
		return nil, fmt.Errorf("core: OperateHost: %w", err)
	}
	idle := c.HostInputIdle
	if idle <= 0 {
		idle = DefaultHostInputIdle
	}
	now := time.Now()
	live, stopLive := context.WithCancel(context.Background())
	o := &hostOp{c: c, pane: *p, person: string(a.caller.Principal), since: now, buffer: buffer, idle: idle,
		live: live, stopLive: stopLive, lastKey: now, stop: make(chan struct{}), done: make(chan struct{})}
	c.hostOps.mu.Lock()
	if c.hostOps.stopped {
		c.hostOps.mu.Unlock()
		stopLive()
		return nil, errors.New("core: OperateHost: the daemon is stopping")
	}
	if busy := c.hostOps.byPane[p.Pane]; busy != nil {
		c.hostOps.mu.Unlock()
		stopLive()
		return nil, &paneBusy{pane: p.Pane, person: busy.person, since: busy.since}
	}
	if c.hostOps.byPane == nil {
		c.hostOps.byPane = map[string]*hostOp{}
	}
	c.hostOps.byPane[p.Pane] = o
	c.hostOps.running.Add(1)
	c.hostOps.mu.Unlock()
	c.Log.Info("host input started", o.logAttrs()...)
	go o.watch(ctx, hostCheckEvery)
	return o, nil
}

// logAttrs name the person and the pane, never a key.
func (o *hostOp) logAttrs() []any {
	return []any{"person", o.person, "pane", o.pane.Pane, "session", o.pane.Session, "window", o.pane.Window, "index", o.pane.Index}
}

// end ends the operation for reason, unless it ended already. The watcher
// does the rest.
func (o *hostOp) end(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reason == "" {
		o.reason = reason
		close(o.stop)
		o.stopLive()
	}
}

func (o *hostOp) ended() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reason != ""
}

// watch ends the operation when ctx ends, when the daemon stops, after
// the idle period, and when a check finds the pane gone or hataori's own;
// then it cleans up.
func (o *hostOp) watch(ctx context.Context, every time.Duration) {
	c := o.c
	tick := time.NewTicker(every)
	defer tick.Stop()
	idle := time.NewTimer(o.idle)
	defer idle.Stop()
	for !o.ended() {
		select {
		case <-o.stop:
		case <-c.Stopping:
			o.end(control.HostStopping)
		case <-ctx.Done():
			if c.stopping() { // the daemon's context ends the connections' too
				o.end(control.HostStopping)
			} else {
				o.end(control.HostClosed)
			}
		case <-idle.C:
			o.mu.Lock()
			left := o.idle - time.Since(o.lastKey)
			o.mu.Unlock()
			if left <= 0 {
				o.end(control.HostIdle)
			} else {
				idle.Reset(left)
			}
		case <-tick.C:
			// A check runs on its own, so that a slow one holds up neither
			// the end nor the next tick's; one at a time.
			if o.checking.CompareAndSwap(false, true) {
				o.checks.Add(1)
				go func() {
					defer o.checks.Done()
					defer o.checking.Store(false)
					o.check()
				}()
			}
		}
	}
	o.finish()
}

// check reads the pane's command and the process table, and ends the
// operation when the pane is gone or hataori's own. A check that fails
// ends nothing; every batch is checked again before it is sent.
func (o *hostOp) check() {
	ctx, cancel := context.WithTimeout(o.live, hostCmdTimeout)
	defer cancel()
	owned, err := o.c.hostPIDs(ctx)
	if err != nil {
		return
	}
	p, own, err := o.c.HostInput.Check(ctx, o.pane.Pane, owned, true)
	switch {
	case err != nil || ctx.Err() != nil:
	case p == nil:
		o.end(control.HostGone)
	case own:
		o.end(control.HostOwn)
	}
}

// finish waits for a batch being sent, deletes the operation's buffer,
// forgets the pane, logs the end and closes Done.
func (o *hostOp) finish() {
	o.checks.Wait() // live has ended, so they stop soon
	o.sendMu.Lock()
	defer o.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), hostCmdTimeout)
	if err := o.c.HostInput.DropBuffer(ctx, o.buffer); err != nil {
		o.c.Log.Error("host input: deleting the operation's tmux buffer", append(o.logAttrs(), "err", err)...)
	}
	cancel()
	o.c.hostOps.mu.Lock()
	if o.c.hostOps.byPane[o.pane.Pane] == o {
		delete(o.c.hostOps.byPane, o.pane.Pane)
	}
	o.c.hostOps.mu.Unlock()
	o.mu.Lock()
	reason := o.reason
	o.mu.Unlock()
	o.c.Log.Info("host input ended", append(o.logAttrs(), "reason", reason)...)
	close(o.done)
	o.c.hostOps.running.Done()
}

// StopHostInput ends every operation of a pane with control.HostStopping,
// refuses every OperateHost after it, and waits, at most for timeout, until each has deleted its buffer and
// logged its end. The daemon calls it as it stops.
func (c *Core) StopHostInput(timeout time.Duration) {
	c.hostOps.mu.Lock()
	c.hostOps.stopped = true // and so nothing is added to running from here
	for _, o := range c.hostOps.byPane {
		o.end(control.HostStopping)
	}
	c.hostOps.mu.Unlock()
	done := make(chan struct{})
	go func() { c.hostOps.running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Send checks the command tmux reports for the pane, then types keys. A
// pane that is gone or hataori's own ends the operation and sends nothing;
// a command that cannot be read sends nothing. When the operation ends
// during the batch, no further key of it is sent.
func (o *hostOp) Send(ctx context.Context, keys []control.HostKey) error {
	o.sendMu.Lock()
	defer o.sendMu.Unlock()
	if o.ended() {
		return fmt.Errorf("core: tmux pane %s: %w", o.pane.Pane, control.ErrEnded)
	}
	if len(keys) == 0 {
		return nil
	}
	for i, k := range keys {
		if k.Key != "" && (k.Text != "" || k.Paste) || k.Key == "" && k.Text == "" ||
			k.Key != "" && !control.ValidHostKey(k.Key) {
			return fmt.Errorf("core: input %d of the batch is neither a key name hataori sends nor a text; nothing was sent", i+1)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, hostCmdTimeout)
	defer cancel()
	defer context.AfterFunc(o.live, cancel)()
	p, own, err := o.c.HostInput.Check(ctx, o.pane.Pane, nil, false)
	switch {
	case err != nil:
		return fmt.Errorf("core: the command of tmux pane %s cannot be read; nothing was sent: %w", o.pane.Pane, err)
	case p == nil:
		o.end(control.HostGone)
		return fmt.Errorf("core: tmux pane %s is gone: %w", o.pane.Pane, control.ErrEnded)
	case own:
		o.end(control.HostOwn)
		return fmt.Errorf("core: tmux pane %s runs one of hataori's processes: %w", o.pane.Pane, control.ErrEnded)
	}
	o.mu.Lock()
	o.lastKey = time.Now()
	o.mu.Unlock()
	if err := o.c.HostInput.Keys(ctx, o.pane.Pane, o.buffer, keys); err != nil {
		return fmt.Errorf("core: tmux pane %s: %w", o.pane.Pane, err)
	}
	return nil
}

func (o *hostOp) Idle() time.Duration   { return o.idle }
func (o *hostOp) Done() <-chan struct{} { return o.done }
func (o *hostOp) End()                  { o.end(control.HostEnded) }

// Reason is empty until Done is closed.
func (o *hostOp) Reason() string {
	select {
	case <-o.done:
	default:
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reason
}
