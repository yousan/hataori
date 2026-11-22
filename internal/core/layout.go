package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// KindLayout is the event of a chat client's layout changing (SetLayout).
const KindLayout control.EventKind = "layout"

// The most of a layout that SetLayout keeps; what is beyond is left out.
// They bound the memory and the answer for a client that reports far more
// than a person could read in a tree.
const (
	MaxLayoutChannels = 2000 // channels of one client, in all its groups
	MaxLayoutThreads  = 5000 // threads of one client, in all its channels
)

// layouts is what each chat client last recorded with SetLayout, by the
// client's name. It lives only in memory: a front end records its layout
// again after each connect.
type layouts struct {
	write sync.Mutex // one SetLayout at a time, so that events follow the order of the changes
	mu    sync.Mutex
	by    map[string][]control.LayoutGroup
}

// SetLayout records how a chat client's service lays out its
// conversations (control.API).
func (a *api) SetLayout(ctx context.Context, client string, groups []control.LayoutGroup) error {
	if err := a.granted("SetLayout", ""); err != nil {
		return err
	}
	scope := ""
	for _, k := range a.caller.Scopes {
		if sc, ok := a.c.Scopes[k]; ok && client != "" && sc.Owner == client {
			scope = k
			break
		}
	}
	if scope == "" {
		a.c.Log.Warn("control call refused", "op", "SetLayout", "principal", string(a.caller.Principal), "client", client,
			"reason", "no scope of the caller's is the client's")
		return fmt.Errorf("core: SetLayout: %w", ErrForbidden)
	}
	if err := a.check(control.OpName, "SetLayout", scope); err != nil {
		return err
	}
	groups = cleanLayout(groups)
	l := &a.c.layouts
	l.write.Lock()
	defer l.write.Unlock()
	l.mu.Lock()
	old, had := l.by[client]
	same := had && sameLayout(old, groups)
	if !same {
		if l.by == nil {
			l.by = map[string][]control.LayoutGroup{}
		}
		l.by[client] = groups
	}
	l.mu.Unlock()
	if same {
		return nil
	}
	// The layout is recorded before its event, so that a reader the event
	// sends to read it again reads this one or a newer. When the event
	// cannot be written, the old layout is put back: no reader would learn
	// of the new one, and the same layout again must write the event.
	b, _ := json.Marshal(map[string]any{"client": client})
	err := a.c.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Append(ctx, control.Event{Kind: KindLayout, Data: b})
		return err
	})
	if err != nil {
		l.mu.Lock()
		if had {
			l.by[client] = old
		} else {
			delete(l.by, client)
		}
		l.mu.Unlock()
	}
	return err
}

// cleanLayout is groups as SetLayout keeps them: names cut to MaxName,
// Scope and Thread cleared, empty lists instead of none, and at most
// MaxLayoutChannels channels and MaxLayoutThreads threads.
func cleanLayout(groups []control.LayoutGroup) []control.LayoutGroup {
	out := make([]control.LayoutGroup, 0, len(groups))
	channels, threads := 0, 0
	for _, g := range groups {
		ng := control.LayoutGroup{ID: g.ID, Name: cutName(g.Name), Channels: []control.LayoutChannel{}}
		for _, ch := range g.Channels {
			if channels == MaxLayoutChannels {
				break
			}
			channels++
			nc := control.LayoutChannel{ID: ch.ID, Name: cutName(ch.Name), URL: ch.URL, Threads: []control.LayoutThread{}}
			for _, th := range ch.Threads {
				if threads == MaxLayoutThreads {
					break
				}
				threads++
				nc.Threads = append(nc.Threads, control.LayoutThread{ID: th.ID, Name: cutName(th.Name), URL: th.URL})
			}
			ng.Channels = append(ng.Channels, nc)
		}
		out = append(out, ng)
	}
	return out
}

// cutName is name without surrounding spaces, cut to MaxName bytes at a
// character.
func cutName(name string) string {
	name = strings.TrimSpace(name)
	for len(name) > MaxName {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

func sameLayout(x, y []control.LayoutGroup) bool {
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(bx) == string(by)
}

// Layout returns each chat client's layout as the caller may see it
// (control.API).
func (a *api) Layout(ctx context.Context) ([]control.ChatLayout, error) {
	if err := a.check(control.OpRead, "Layout", ""); err != nil {
		return nil, err
	}
	l := &a.c.layouts
	l.mu.Lock()
	clients := make([]string, 0, len(l.by))
	recorded := make(map[string][]control.LayoutGroup, len(l.by))
	for c, gs := range l.by {
		clients = append(clients, c)
		recorded[c] = gs // never changed in place: SetLayout replaces it
	}
	l.mu.Unlock()
	slices.Sort(clients)
	owner := a.c.People.Owner()
	isOwner := owner != "" && a.caller.Principal == control.Principal(owner)
	out := []control.ChatLayout{}
	for _, client := range clients {
		linked, err := a.c.linkedThreads(ctx, client)
		if err != nil {
			return nil, err
		}
		cl := control.ChatLayout{Client: client, Groups: []control.LayoutGroup{}}
		for _, g := range recorded[client] {
			ng := control.LayoutGroup{ID: g.ID, Name: a.layoutText(g.Name), Channels: []control.LayoutChannel{}}
			for _, ch := range g.Channels {
				scope := ""
				if sc, ok := a.c.Scopes[ch.ID]; ok && sc.Owner == client && a.sees(ch.ID) {
					scope = ch.ID
				}
				if scope == "" && !isOwner {
					continue // a channel of no scope the caller may see
				}
				nc := control.LayoutChannel{ID: ch.ID, Name: a.layoutText(ch.Name), URL: a.layoutText(ch.URL), Scope: scope, Threads: []control.LayoutThread{}}
				for _, th := range ch.Threads {
					nt := control.LayoutThread{ID: th.ID, Name: a.layoutText(th.Name), URL: a.layoutText(th.URL)}
					if t, ok := linked[[2]string{ch.ID, th.ID}]; ok && a.sees(t.scope) {
						nt.Thread = t.id
					}
					nc.Threads = append(nc.Threads, nt)
				}
				ng.Channels = append(ng.Channels, nc)
			}
			if len(ng.Channels) == 0 && !isOwner {
				continue // a group that holds none of the caller's channels
			}
			cl.Groups = append(cl.Groups, ng)
		}
		out = append(out, cl)
	}
	return out, nil
}

// layoutText is a name or a URL of a layout as the caller may read it. A
// URL is text too: a surface may put a conversation's name in it.
func (a *api) layoutText(s string) string {
	if a.caller.NoText {
		return ""
	}
	return s
}

// linkedThread is the thread a conversation is linked to, and its scope.
type linkedThread struct {
	id    control.ThreadID
	scope string
}

// linkedThreads is every conversation of client in a thread that is linked
// to a thread, by channel and chat thread.
func (c *Core) linkedThreads(ctx context.Context, client string) (map[[2]string]linkedThread, error) {
	rows, err := c.Store.DB.QueryContext(ctx, `SELECT l.channel, l.thread, l.thread_id, t.scope FROM links l
		JOIN threads t ON t.id = l.thread_id WHERE l.client = ? AND l.thread != ''`, client)
	if err != nil {
		return nil, fmt.Errorf("core: layout: %w", err)
	}
	defer rows.Close()
	out := map[[2]string]linkedThread{}
	for rows.Next() {
		var ch, th string
		var t linkedThread
		var id int64
		if err := rows.Scan(&ch, &th, &id, &t.scope); err != nil {
			return nil, fmt.Errorf("core: layout: %w", err)
		}
		t.id = control.ThreadID(id)
		out[[2]string{ch, th}] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: layout: %w", err)
	}
	return out, nil
}
