package core

import (
	"context"
	"slices"
	"strings"

	"github.com/yousan/hataori/internal/control"
)

// Clients reports the connection of each client that owns a scope the
// caller may see and says whether it is connected (control.Connection).
func (a *api) Clients(ctx context.Context) ([]control.ClientState, error) {
	if err := a.check(control.OpRead, "Clients", ""); err != nil {
		return nil, err
	}
	out := []control.ClientState{}
	for _, key := range a.caller.Scopes {
		sc, ok := a.c.Scopes[key]
		if !ok || sc.Owner == "" || !a.personSees(key) {
			continue
		}
		if slices.ContainsFunc(out, func(s control.ClientState) bool { return s.Client == sc.Owner }) {
			continue
		}
		conn, ok := a.c.Owners[sc.Owner].(control.Connection)
		if !ok {
			continue
		}
		up, since := conn.State()
		out = append(out, control.ClientState{Client: sc.Owner, Connected: up, Since: since})
	}
	slices.SortFunc(out, func(x, y control.ClientState) int { return strings.Compare(x.Client, y.Client) })
	return out, nil
}
