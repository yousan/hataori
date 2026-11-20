package tmux

import "testing"

// How a dead pane's agent ended, as list-panes prints it: an exit status of
// 0 included, which a tmux condition reads as false, and a signal by number
// or by tmux's name, recorded as hataori's own host records it.
func TestStatusOfADeadPane(t *testing.T) {
	for _, c := range []struct {
		status, signal string
		code           int
		sig            string
	}{
		{"x0", "-", 0, ""},
		{"x3", "-", 3, ""},
		{"x", "term", -1, "terminated"},
		{"x", "15", -1, "terminated"},
		{"x", "SIGKILL", -1, "killed"},
	} {
		st := statusOf(pane{dead: true, status: c.status, signal: c.signal})
		if !st.Exited || st.Code != c.code || st.Signal != c.sig {
			t.Errorf("%q %q: %+v, want code %d signal %q", c.status, c.signal, st, c.code, c.sig)
		}
	}
}
