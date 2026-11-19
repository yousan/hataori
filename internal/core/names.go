package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// KindNamed is the event of a conversation's name changing (SetName).
const KindNamed control.EventKind = "named"

// MaxName is the most bytes of a conversation's name that are kept.
const MaxName = 256

// SetName records the name the chat service shows for a conversation
// (control.API).
func (a *api) SetName(ctx context.Context, l control.LinkRef, name string) error {
	name = strings.TrimSpace(name)
	for len(name) > MaxName {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	scope, thread, err := a.c.nameScope(ctx, l)
	if err != nil {
		return err
	}
	if err := a.check(control.OpName, "SetName", scope); err != nil {
		return err
	}
	return a.c.Store.Update(ctx, func(tx *store.Tx) error {
		var old string
		err := tx.QueryRowContext(ctx, `SELECT name FROM names WHERE client = ? AND channel = ? AND thread = ?`,
			l.Client, l.Channel, l.Thread).Scan(&old)
		switch {
		case err == nil && old == name, errors.Is(err, sql.ErrNoRows) && name == "":
			return nil // nothing changes
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("core: set name: %w", err)
		}
		if name == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM names WHERE client = ? AND channel = ? AND thread = ?`, l.Client, l.Channel, l.Thread)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO names (client, channel, thread, name, updated_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (client, channel, thread) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at`,
				l.Client, l.Channel, l.Thread, name, time.Now().UnixMilli())
		}
		if err != nil {
			return fmt.Errorf("core: set name: %w", err)
		}
		d := map[string]any{"client": l.Client, "channel": l.Channel}
		if l.Thread != "" {
			d["thread"] = l.Thread
		}
		if name != "" {
			d["name"] = name
		}
		b, _ := json.Marshal(d)
		_, err = tx.Append(ctx, control.Event{Thread: thread, Scope: scope, Kind: KindNamed, Data: b})
		return err
	})
}

// nameScope is the scope of a conversation SetName names: the scope whose
// own channel it is, or the scope of the thread linked to it, with that
// thread. A conversation of no scope is ErrNotFound.
func (c *Core) nameScope(ctx context.Context, l control.LinkRef) (string, control.ThreadID, error) {
	if l.Client == "" || l.Channel == "" {
		return "", 0, fmt.Errorf("core: set name: a conversation needs a client and a channel: %w", ErrNotFound)
	}
	if l.Thread == "" {
		if sc, ok := c.Scopes[l.Channel]; ok && sc.Owner == l.Client {
			return l.Channel, 0, nil
		}
		return "", 0, fmt.Errorf("core: set name: %s %s is no scope's channel: %w", l.Client, l.Channel, ErrNotFound)
	}
	var id int64
	err := c.Store.DB.QueryRowContext(ctx, `SELECT thread_id FROM links WHERE client = ? AND channel = ? AND thread = ?`,
		l.Client, l.Channel, l.Thread).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("core: set name: %s %s/%s is linked to no thread: %w", l.Client, l.Channel, l.Thread, ErrNotFound)
	}
	if err != nil {
		return "", 0, fmt.Errorf("core: set name: %w", err)
	}
	scope, err := c.scopeOf(ctx, c.Store.DB, control.ThreadID(id))
	return scope, control.ThreadID(id), err
}

// scopeNames fills the ScopeName of each thread in ts: the config's name
// of its scope, else its channel's name as the chat service shows it,
// which a caller that may not read text does not get.
func (a *api) scopeNames(ctx context.Context, ts []control.ThreadInfo) error {
	known := map[string]string{}
	for i := range ts {
		scope := ts[i].Scope
		name, ok := known[scope]
		if !ok {
			sc := a.c.Scopes[scope]
			name = sc.Name
			if name == "" && !a.caller.NoText && sc.Owner != "" {
				err := a.c.Store.DB.QueryRowContext(ctx, `SELECT name FROM names WHERE client = ? AND channel = ? AND thread = ''`,
					sc.Owner, scope).Scan(&name)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("core: scope name: %w", err)
				}
			}
			known[scope] = name
		}
		ts[i].ScopeName = name
	}
	return nil
}
