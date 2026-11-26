package host

import "slices"

// attachmentPIDs snapshots connections, including repeated PIDs. Unknown
// identities make the whole snapshot unsuitable for host input checks.
func (h *host) attachmentPIDs() ([]int, bool, int64) {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	pids := make([]int, 0, len(h.attachments))
	known := true
	for _, pid := range h.attachments {
		pids = append(pids, pid)
		known = known && pid > 0
	}
	slices.Sort(pids)
	return pids, known, h.detached.Load()
}
