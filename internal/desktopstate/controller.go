package desktopstate

import (
	"context"
	"errors"
	"time"
)

// Child is an owned launch handle, never a PID recovered from disk. Stop closes
// only its lifetime pipe. Done closes only after its exit has been joined.
type Child interface {
	Stop() error
	Done() <-chan struct{}
}

type Launch func(context.Context, Selection) (Child, error)

// Controller is confined to one launcher's serial loop. Persisted state is
// always read before an action. A replacement cannot start until the old
// daemon joins, and failure cannot turn a persisted stop back into a start.
type Controller struct {
	Read   func(context.Context) (Selection, error)
	Launch Launch
	Now    func() time.Time
	child  Child
	active Selection
	ending bool
	fails  int
	next   time.Time
}

func (c *Controller) Tick(ctx context.Context) error {
	if c.Now == nil {
		c.Now = time.Now
	}
	d, err := c.Read(ctx)
	if err != nil {
		return err // uncertain persisted state never permits a start
	}
	if c.child != nil {
		select {
		case <-c.child.Done():
			c.child = nil
			if c.ending {
				c.fails = 0
			} else {
				c.fails++
				c.next = c.Now().Add(backoff(c.fails))
			}
			c.ending = false
		default:
		}
	}
	if c.child != nil {
		if !c.ending && (d.Intent == Stopped || d.Version != c.active.Version || d.Binary != c.active.Binary) {
			c.ending = true
			return c.child.Stop()
		}
		return nil
	}
	if d.Intent == Stopped || c.Now().Before(c.next) {
		return nil
	}
	if c.fails >= 5 {
		return errors.New("desktop: repeated daemon failure; explicit intervention required")
	}
	if err := validSelection(d); err != nil {
		return err
	}
	c.child, err = c.Launch(ctx, d)
	if err != nil {
		c.fails++
		c.next = c.Now().Add(backoff(c.fails))
		return nil
	}
	c.active = d
	return nil
}

// Close never ends session hosts, removes locks, restores data or kills a
// process tree. If the owned daemon cannot join, report that fact and retain
// its data and the daemon's own lock rather than forcing a replacement.
func (c *Controller) Close(ctx context.Context) error {
	if c.child == nil {
		return nil
	}
	if !c.ending {
		if err := c.child.Stop(); err != nil {
			return err
		}
	}
	select {
	case <-c.child.Done():
		c.child = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func backoff(failures int) time.Duration {
	return time.Second * time.Duration(1<<min(failures, 5))
}
