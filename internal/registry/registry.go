// Package registry keeps named implementations, such as surface drivers and
// agent adapters, so that the core can find them without importing them.
package registry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry maps names to values of one kind. The zero value is ready to use
// and safe for concurrent use.
type Registry[T any] struct {
	mu    sync.RWMutex
	items map[string]T
}

// Register stores v under name. It fails when the name is already taken or
// is not usable as a key and a path segment.
func (r *Registry[T]) Register(name string, v T) error {
	if err := ValidName(name); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[name]; ok {
		return fmt.Errorf("registry: %q is already registered", name)
	}
	if r.items == nil {
		r.items = make(map[string]T)
	}
	r.items[name] = v
	return nil
}

// Lookup returns the value registered under name, and whether there is one.
func (r *Registry[T]) Lookup(name string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[name]
	return v, ok
}

// Names returns the registered names in sorted order.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.items))
	for n := range r.items {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ValidName reports whether name can be used as a registry key and as one
// path segment: lower-case ASCII letters, digits and '-', starting with a
// letter.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("registry: empty name")
	}
	if c := name[0]; c < 'a' || c > 'z' {
		return fmt.Errorf("registry: name %q must start with a lower-case letter", name)
	}
	if i := strings.IndexFunc(name, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-')
	}); i >= 0 {
		return fmt.Errorf("registry: name %q may hold only a-z, 0-9 and '-'", name)
	}
	return nil
}
