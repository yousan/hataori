package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Link shows thread id in one more conversation (design, section 5.2).
func (a *api) Link(ctx context.Context, id control.ThreadID, l control.LinkRef) (control.LinkRef, error) {
	scope, err := a.actThread(ctx, control.OpSend, "Link", id)
	if err != nil {
		return control.LinkRef{}, err
	}
	if l.Client == "" || l.Channel == "" {
		return control.LinkRef{}, fmt.Errorf("core: link thread %d: a link names a client and a channel", id)
	}
	sc := a.c.Scopes[scope]
	own := l.Client == sc.Owner && l.Channel == scope
	if !own && !slices.Contains(sc.ShowIn, l.Client+":"+l.Channel) {
		a.c.Log.Warn("link refused", "thread", int64(id), "client", l.Client, "channel", l.Channel,
			"scope", scope, "reason", "the channel is neither the scope's own nor in its show_in")
		return control.LinkRef{}, fmt.Errorf("core: link thread %d in %s:%s: not in the show_in of scope %q: %w",
			id, l.Client, l.Channel, scope, ErrForbidden)
	}
	// One link at a time per channel: two attaches of one thread there
	// open one conversation.
	defer a.c.locks.lock("link:" + l.Client + "\x00" + l.Channel)()
	db := a.c.Store.DB
	if l.Thread != "" {
		var other int64
		err := db.QueryRowContext(ctx, `SELECT thread_id FROM links WHERE client = ? AND channel = ? AND thread = ?`,
			l.Client, l.Channel, l.Thread).Scan(&other)
		switch {
		case err == nil && control.ThreadID(other) == id:
			return l, nil // linked already: nothing changes
		case err == nil:
			a.c.Log.Warn("link refused", "thread", int64(id), "client", l.Client, "channel", l.Channel,
				"reason", fmt.Sprintf("the conversation shows thread %d", other))
			return control.LinkRef{}, fmt.Errorf("core: link thread %d in %s:%s:%s: %w", id, l.Client, l.Channel, l.Thread,
				control.ErrLinkedElsewhere)
		case !errors.Is(err, sql.ErrNoRows):
			return control.LinkRef{}, fmt.Errorf("core: link thread %d: %w", id, err)
		}
	} else {
		// A conversation of the thread in that channel already: it is shown
		// there, and nothing is opened.
		var have control.LinkRef
		err := db.QueryRowContext(ctx, `SELECT client, channel, thread FROM links WHERE thread_id = ? AND client = ? AND channel = ?
			ORDER BY id LIMIT 1`, int64(id), l.Client, l.Channel).Scan(&have.Client, &have.Channel, &have.Thread)
		if err == nil {
			return have, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return control.LinkRef{}, fmt.Errorf("core: link thread %d: %w", id, err)
		}
		owner, ok := a.c.Owners[l.Client]
		if !ok {
			return control.LinkRef{}, fmt.Errorf("core: link thread %d: no client %q opens conversations", id, l.Client)
		}
		var title string
		if err := db.QueryRowContext(ctx, `SELECT title FROM threads WHERE id = ?`, int64(id)).Scan(&title); err != nil {
			return control.LinkRef{}, fmt.Errorf("core: link thread %d: %w", id, err)
		}
		if title == "" {
			title = fmt.Sprintf("#%d", id)
		}
		opened, err := owner.NewThread(ctx, l.Channel, "", title)
		if err != nil {
			return control.LinkRef{}, fmt.Errorf("core: link thread %d: %s: %w", id, l.Client, err)
		}
		if opened.Client != l.Client || opened.Channel != l.Channel || opened.Thread == "" {
			return control.LinkRef{}, fmt.Errorf("core: link thread %d: %s opened a conversation elsewhere: %s %s %s",
				id, l.Client, opened.Client, opened.Channel, opened.Thread)
		}
		l = opened
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO links (thread_id, client, channel, thread, made_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		int64(id), l.Client, l.Channel, l.Thread, string(a.caller.Principal), time.Now().UnixMilli()); err != nil {
		return control.LinkRef{}, fmt.Errorf("core: link thread %d: %w", id, err)
	}
	a.c.Log.Info("thread linked", "thread", int64(id), "client", l.Client, "channel", l.Channel, "conversation", l.Thread,
		"person", string(a.caller.Principal))
	return l, nil
}
