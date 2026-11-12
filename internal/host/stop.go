package host

import (
	"errors"
	"strings"
	"syscall"
	"time"
)

// killWait bounds the wait for a process group to vanish after SIGKILL.
const killWait = 10 * time.Second

// stop ends the command's process group: SIGINT, then SIGTERM after the
// interrupt grace, then SIGKILL after the terminate grace. It stops sending
// as soon as no process of the group is left, and answers only then. The
// caller calls h.stops.Done once the answer is sent when registered is
// true, so the host does not exit before.
func (h *host) stop(req request) (resp response, registered bool) {
	h.stopMu.Lock()
	if h.closing {
		h.stopMu.Unlock()
		return response{OK: true, Already: true}, false
	}
	h.stops.Add(1)
	h.stopMu.Unlock()
	return h.signalGroup(req), true
}

func (h *host) signalGroup(req request) response {
	pgid := h.cmd.Process.Pid
	if groupGone(pgid) {
		return response{OK: true, Already: true}
	}
	var sent []string
	steps := []struct {
		sig   syscall.Signal
		name  string
		grace time.Duration
	}{
		{syscall.SIGINT, "SIGINT", req.Interrupt},
		{syscall.SIGTERM, "SIGTERM", req.Terminate},
		{syscall.SIGKILL, "SIGKILL", killWait},
	}
	for _, s := range steps {
		if err := syscall.Kill(-pgid, s.sig); errors.Is(err, syscall.ESRCH) {
			break
		}
		sent = append(sent, s.name)
		if waitGone(pgid, s.grace) {
			break
		}
	}
	note := strings.Join(sent, ",")
	h.stopMu.Lock()
	h.stopNote = note
	h.stopMu.Unlock()
	if !groupGone(pgid) {
		return response{Error: "processes of the group survived SIGKILL", Signals: note}
	}
	return response{OK: true, Signals: note}
}

// groupGone reports whether no process of the group exists.
func groupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

func waitGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if groupGone(pgid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
