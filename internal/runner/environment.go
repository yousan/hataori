package runner

import (
	"slices"
	"sort"
	"strings"
)

// repositoryEnv replaces all occurrences of configured names, without
// changing the adapter's slice or putting values in the stored agent spec.
func repositoryEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	env := slices.DeleteFunc(slices.Clone(base), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		_, ok := extra[name]
		return ok
	})
	var names []string
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+extra[name])
	}
	return env
}
