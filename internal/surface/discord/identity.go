package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// identity lets no request leave with the token before the token is known
// to be the configured bot's. It is the transport of the driver's REST
// client, so that no call can go around it.
type identity struct {
	base http.RoundTripper
	// ask asks the service which bot the token is for.
	ask func(ctx context.Context) (snowflake.ID, error)
	bot snowflake.ID // the configured bot

	checking chan struct{} // holds a value while one check is made
	passed   atomic.Bool
	other    atomic.Pointer[snowflake.ID] // the other bot the token is for, for good
}

// asking marks the context of the one request that is made before the
// check has passed.
type asking struct{}

// newClient returns a REST client that uses the token as bot only, and the
// identity check in front of it.
func newClient(bot snowflake.ID, token string, opts ...rest.ClientConfigOpt) (rest.Client, *identity) {
	i := &identity{base: http.DefaultTransport, bot: bot, checking: make(chan struct{}, 1)}
	// The library's own timeout (rest.defaultClientConfig).
	c := rest.NewClient(token, append(opts, rest.WithHTTPClient(&http.Client{Transport: i, Timeout: 20 * time.Second}))...)
	i.ask = func(ctx context.Context) (snowflake.ID, error) {
		var me struct {
			ID snowflake.ID `json:"id"`
		}
		err := c.Do(rest.GetCurrentUser.Compile(nil), nil, &me, rest.WithCtx(context.WithValue(ctx, asking{}, true)))
		return me.ID, err
	}
	return c, i
}

// RoundTrip refuses a request while the check fails, without sending it. A
// check that could not be made leaves an error with no answer of the
// service, so that the call may be made again.
func (i *identity) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Context().Value(asking{}) == nil {
		if err := i.check(r.Context()); err != nil {
			if r.Body != nil {
				r.Body.Close()
			}
			return nil, err
		}
	}
	return i.base.RoundTrip(r)
}

// check returns nil once the token is known to be the configured bot's. It
// asks the service until it has an answer; after that, never again.
func (i *identity) check(ctx context.Context) error {
	if i.passed.Load() {
		return i.refused()
	}
	select {
	case i.checking <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-i.checking }()
	if err := i.refused(); err != nil || i.passed.Load() {
		return err
	}
	id, err := i.ask(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Not wrapped: an answer to the check is none to the request that
		// waits for it.
		return fmt.Errorf("check the bot's identity: %v", err)
	}
	if id == 0 { // an answer that names no bot, as of a proxy in between
		return errors.New("check the bot's identity: the answer names no bot")
	}
	if id != i.bot {
		i.refuse(id)
		return i.refused()
	}
	i.passed.Store(true)
	return nil
}

// refuse records that the token is for bot id, which is not the configured
// one.
func (i *identity) refuse(id snowflake.ID) { i.other.CompareAndSwap(nil, &id) }

// refused is the error of every call once the token is known to be another
// bot's, and nil otherwise.
func (i *identity) refused() error {
	if id := i.other.Load(); id != nil {
		return fmt.Errorf("the token is for bot %s, but the config names bot %s: %w", *id, i.bot, surface.ErrOtherIdentity)
	}
	return nil
}

// CheckIdentity checks that the token is the configured bot's
// (surface.Identified).
func (d *Driver) CheckIdentity(ctx context.Context) error {
	if err := d.identity.check(ctx); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	return nil
}
