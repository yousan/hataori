// Package people resolves the ids that chat services and automated posters
// give to the people of an instance's config, and says who may act in a
// scope (design, section 5.5).
//
// An id is "<client>:<id>": a client's registered name, a colon, and the
// service's own id of the author. An id that no person lists is a guest.
// Ids are linked to people only by the config.
package people

import (
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
