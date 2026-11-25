package hostterm

import (
	"slices"
	"strings"
)

// matchesOwn reports whether comm, a command name as ps or tmux reports
// it, is one of own. Linux cuts a command name to 15 bytes.
func matchesOwn(comm string, own []string) bool {
	return slices.ContainsFunc(own, func(name string) bool {
		return name != "" && (comm == name || len(comm) == 15 && strings.HasPrefix(name, comm))
	})
}

// ownPIDs are the processes with one of the names own that hold no terminal.
func ownPIDs(ps []proc, own []string) []int {
	var pids []int
	for _, p := range ps {
		if p.tty == "" && matchesOwn(p.comm, own) {
			pids = append(pids, p.pid)
		}
	}
	return pids
}
