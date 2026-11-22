package chat

import (
	"context"
	"errors"
	"sync"
	"time"
)

// conn is whether the driver is connected, and since when it is or is not.
type conn struct {
	mu    sync.Mutex
	up    bool
	since time.Time
	// connectedAt is when the driver last said it connected.
	connectedAt time.Time
	// failures counts the driver's Runs that failed since the last line
	// that said it connected; logged is when the last line about a failure
	// was logged, and told whether that line has been followed by one
	// that says it connected. A connection that keeps dropping logs no
	// more than one that never connects.
	failures int
	logged   time.Time
	told     bool
}

func (c *conn) down(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.up || c.since.IsZero() {
		c.up, c.since = false, now
	}
}

// State reports whether the driver is connected now, and since when it is,
// or is not (control.Connection).
func (f *FrontEnd) State() (bool, time.Time) {
	f.conn.mu.Lock()
	defer f.conn.mu.Unlock()
	return f.conn.up, f.conn.since
}

// Disconnected records that the driver lost its connection and is
// connecting again by itself (surface.Disconnected).
func (f *FrontEnd) Disconnected() { f.conn.down(f.now()) }

// connected records that the driver connected, and logs one line when a
// line about a failure was logged since the last such line.
func (f *FrontEnd) connected() {
	c := &f.conn
	now := f.now()
	c.mu.Lock()
	if !c.up {
		c.up, c.since = true, now
	}
	c.connectedAt = now
	failures, tell := c.failures, c.told
	if tell {
		c.failures, c.told = 0, false
	}
	c.mu.Unlock()
	if tell {
		f.Log.Info("chat front end connected", "client", f.Name, "failures", failures)
	}
}

// runDriver runs the driver until ctx ends. When its Run returns before,
// as when its first request fails because the network is not up yet, or
// when a dropped connection cannot be made again, it is run again after a
// delay that starts at RetryFirst and doubles at each failure up to
// RetryMax; it starts at RetryFirst again after a Run that stayed
// connected for RetryMax. While it keeps failing, it logs at most one line
// per LogEvery.
func (f *FrontEnd) runDriver(ctx context.Context) {
	first, most, every := f.RetryFirst, f.RetryMax, f.LogEvery
	if first <= 0 {
		first = time.Second
	}
	if most <= 0 {
		most = time.Minute
	}
	if every <= 0 {
		every = time.Minute
	}
	delay := first
	for {
		start := f.now()
		err := f.Driver.Run(ctx, f)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the driver stopped")
		}
		now := f.now()
		c := &f.conn
		c.mu.Lock()
		if at := c.connectedAt; !at.Before(start) && now.Sub(at) >= most {
			delay = first // it stayed connected a while
		}
		c.failures++
		failures := c.failures
		log := c.logged.IsZero() || now.Sub(c.logged) >= every
		if log {
			c.logged, c.told = now, true
		}
		c.mu.Unlock()
		c.down(now)
		if log {
			f.Log.Error("chat front end: the driver stopped; trying again", "client", f.Name, "failures", failures,
				"retry_in", delay.String(), "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, most)
	}
}
