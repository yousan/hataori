// Package people resolves the ids that chat services and automated posters
// give to the people of an instance's config, and says who may act in a
// scope (design, section 5.5).
//
// An id is "<client>:<id>": a client's registered name, a colon, and the
// service's own id of the author. An id that no person lists is a guest.
// Ids are linked to people only by the config.
package people

import (
	"maps"
	"slices"
	"strings"
)

// Person is one entry of the config's people.
type Person struct {
	Name string
	IDs  []string // "<client>:<id>"
}

// Directory is the people of one instance, the owner, and the people of
// each scope. It does not change once made.
type Directory struct {
	owner  string
	byID   map[string]string          // id → name
	ids    map[string][]string        // name → ids, in config order
	scopes map[string]map[string]bool // scope → names
}

// New makes a Directory. The caller has checked the config: every name and
// id is listed once, and owner and every scope's names are listed people.
func New(people []Person, owner string, scopes map[string][]string) *Directory {
	d := &Directory{owner: owner, byID: map[string]string{}, ids: map[string][]string{}, scopes: map[string]map[string]bool{}}
	for _, p := range people {
		d.ids[p.Name] = slices.Clone(p.IDs)
		for _, id := range p.IDs {
			d.byID[id] = p.Name
		}
	}
	for scope, names := range scopes {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		d.scopes[scope] = m
	}
	return d
}

// Split splits an id into its client and the service's own id. ok is false
// when id has no colon, or nothing on either side of it.
func Split(id string) (client, local string, ok bool) {
	client, local, ok = strings.Cut(id, ":")
	return client, local, ok && client != "" && local != ""
}

// Join makes the id of a service's own id on client.
func Join(client, local string) string { return client + ":" + local }

// Resolve returns the person id belongs to. ok is false for a guest.
func (d *Directory) Resolve(id string) (name string, ok bool) {
	if d == nil {
		return "", false
	}
	name, ok = d.byID[id]
	return name, ok
}

// OldOwner is the name an old-form config, one without people whose owner
// and posters are raw ids, gives its owner.
const OldOwner = "owner"

// OldPoster is the name an old-form config gives the poster local on
// client: "<client>/<id>". A client name has no "/", so no two ids get one
// name.
func OldPoster(client, local string) string { return client + "/" + local }

// Recorded names the person that by, who acted as an older hataori
// recorded it (a thread's started_by), stands for, with client the client
// of the thread's scope. A row has held, over time:
//   - the person's name, today and with an old-form config, whose owner is
//     OldOwner and whose posters are OldPoster names;
//   - before callers were resolved to people, the chat service's id of
//     the author without its client, or the old-form owner's raw id.
//
// A name of today's people is kept; an id, in full or bare on client, or an
// OldPoster name names the person who lists that id. OldOwner names no
// one: the owner then may not be today's owner. ok is false when no person
// is found, and by is then all there is.
func (d *Directory) Recorded(by, client string) (name string, ok bool) {
	switch {
	case d == nil || by == "":
		return "", false
	case d.Listed(by):
		return by, true
	}
	if name, ok := d.Resolve(by); ok {
		return name, true
	}
	if c, local, found := strings.Cut(by, "/"); found && c != "" && local != "" {
		if name, ok := d.Resolve(Join(c, local)); ok {
			return name, true
		}
	}
	if client != "" {
		return d.Resolve(Join(client, by))
	}
	return "", false
}

// Listed reports whether name is the name of a person.
func (d *Directory) Listed(name string) bool {
	if d == nil {
		return false
	}
	_, ok := d.ids[name]
	return ok
}

// Owner returns the owner's name.
func (d *Directory) Owner() string {
	if d == nil {
		return ""
	}
	return d.owner
}

// May reports whether the person name may act in scope: the owner may act
// in every scope, anyone else in the scopes that list them. The empty name,
// a guest's, may act nowhere.
func (d *Directory) May(name, scope string) bool {
	if d == nil || name == "" {
		return false
	}
	return name == d.owner || d.scopes[scope][name]
}

// Of returns the names of the people who may act in scope: the owner first,
// then the scope's people, by name.
func (d *Directory) Of(scope string) []string {
	if d == nil {
		return nil
	}
	var out []string
	if d.owner != "" {
		out = append(out, d.owner)
	}
	for _, n := range slices.Sorted(maps.Keys(d.scopes[scope])) {
		if n != d.owner {
			out = append(out, n)
		}
	}
	return out
}

// IDs returns the service's own ids of the person name on client, in config
// order, without the client's prefix.
func (d *Directory) IDs(name, client string) []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, id := range d.ids[name] {
		if c, local, ok := Split(id); ok && c == client {
			out = append(out, local)
		}
	}
	return out
}
