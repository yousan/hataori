package instance

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yousan/hataori/internal/workspace"
)

func repositories(entries []Repository) ([]Repository, error) {
	seen := map[string]bool{}
	for i := range entries {
		r := &entries[i]
		key := fmt.Sprintf("repositories[%d]", i)
		r.URL = strings.TrimSpace(r.URL)
		if err := workspace.CheckAddress(r.URL); err != nil {
			return nil, fmt.Errorf("config: key %q: %v", key+".url", err)
		}
		if seen[r.URL] {
			return nil, fmt.Errorf("config: key %q: repository is listed twice", key+".url")
		}
		seen[r.URL] = true
		var names []string
		for name := range r.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			k := fmt.Sprintf("%s.env[%q]", key, name)
			if name == "" || strings.ContainsAny(name, "=\x00") || strings.HasPrefix(name, "HATAORI_") {
				return nil, fmt.Errorf("config: key %s: invalid or reserved environment variable name", k)
			}
			if strings.ContainsRune(r.Env[name], '\x00') {
				return nil, fmt.Errorf("config: key %s: environment value contains a NUL", k)
			}
		}
	}
	return entries, nil
}
