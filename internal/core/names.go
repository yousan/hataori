package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// Scopes lists the scopes the caller may see, with their names
// (control.API).
func (a *api) Scopes(ctx context.Context) ([]control.ScopeInfo, error) {
	if err := a.check(control.OpRead, "Scopes", ""); err != nil {
		return nil, err
	}
	var out []control.ScopeInfo
	for _, key := range a.caller.Scopes {
		sc, ok := a.c.Scopes[key]
		if !ok || !a.personSees(key) {
			continue
		}
		name := sc.Name
		if name == "" && !a.caller.NoText && sc.Owner != "" {
			err := a.c.Store.DB.QueryRowContext(ctx, `SELECT name FROM names WHERE client = ? AND channel = ? AND thread = ''`,
				sc.Owner, key).Scan(&name)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("core: scopes: %w", err)
			}
		}
		out = append(out, control.ScopeInfo{Key: key, Name: name})
	}
	slices.SortFunc(out, func(x, y control.ScopeInfo) int { return strings.Compare(x.Key, y.Key) })
	return out, nil
}
