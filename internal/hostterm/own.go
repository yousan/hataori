package hostterm

import (
	"path"
	"slices"
	"strings"
)

// matchesOwn reports whether comm, a command name as ps or tmux reports
// it, is one of own. Linux cuts a command name to 15 bytes; tmux reports a
// command started by a relative path as that path; and a file system may
// find a file under its name in another case.
func matchesOwn(comm string, own []string) bool {
	base := path.Base(comm)
	return slices.ContainsFunc(own, func(name string) bool {
		return strings.EqualFold(base, name) || len(comm) == 15 && strings.HasPrefix(name, comm)
	})
}

// ownPIDs are the processes with one of the names own that hold no terminal.
// PID 1 also adopts unrelated orphans, so its descendants are not ours.
// Session hosts are still found individually, including before they are recorded.
func ownPIDs(ps []proc, own []string) []int {
	var pids []int
	for _, p := range ps {
		if p.pid != 1 && p.tty == "" && matchesOwn(p.comm, own) {
			pids = append(pids, p.pid)
		}
	}
	return pids
}
