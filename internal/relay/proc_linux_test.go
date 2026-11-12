package relay

import (
	"syscall"
	"testing"
)

// The kernel kills the login command when the daemon dies.
func TestLoginCommandDiesWithTheDaemon(t *testing.T) {
	a := procAttr()
	if !a.Setpgid || a.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("process attributes %+v", a)
	}
}
