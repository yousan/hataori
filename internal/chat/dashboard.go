package chat

import (
	"context"
	"errors"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// dashboardCommand asks for a dashboard sign-in link (design, sections 7.2
// and 10): `/dashboard`, or `<trigger> dashboard` on a surface without
// commands. Who may get one is the core's to decide (SignInLink); the front
// end checks nothing of its own.
const dashboardCommand = "dashboard"

// dashboardLink makes a sign-in link for the author of c and sends it to
// them in a direct conversation. The link is posted at once, as the login
// relay's is (section 4.7): never through the outbox, never logged, never
// in the conversation c was used in, which gets nothing. A refused request
// sends nothing anywhere; the core logs it. Each command makes one link
// however often it is delivered.
func (f *FrontEnd) dashboardLink(ctx context.Context, c surface.Command) error {
	first, err := f.once(ctx, c.Conv.Channel, "dashboard:"+c.ID, true)
	if err != nil || !first {
		return err
	}
	api := f.API(control.Caller{From: f.from(c.Author), May: control.OpSignIn})
	link, err := api.SignInLink(ctx, f.SignInWay)
	switch {
	case errors.Is(err, control.ErrForbidden):
		return nil // the core logged it
	case errors.Is(err, control.ErrNoDashboard):
		return f.answer(ctx, c, "This hataori instance has no dashboard.")
	case err != nil:
		f.Log.Error("dashboard link: not made", "client", f.Name, "id", c.Author.ID, "err", err)
		return f.answer(ctx, c, "No link: an error, which is in the log.")
	}
	conv, err := f.Driver.OpenDirect(ctx, c.Author.ID)
	if err == nil {
		_, err = f.Driver.Post(ctx, conv, "", surface.Message{Text: "Your dashboard sign-in link. It works once, and only for a short while:\n" + link})
	}
	if err != nil {
		// The error is the service's, not the link's.
		f.Log.Warn("dashboard link: not sent", "client", f.Name, "id", c.Author.ID, "err", err)
		return f.answer(ctx, c, "No link: a direct message could not be sent to you.")
	}
	f.Log.Info("dashboard link sent", "client", f.Name, "id", c.Author.ID)
	return nil
}
