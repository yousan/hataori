package host

import (
	"bytes"
	"net"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The host records in host.log how it started and ended, how its command
// ended, a connection it dropped and why, and a signal that ended it.
func TestHostLogRecordsWhy(t *testing.T) {
	t.Run("command ends", func(t *testing.T) {
		dir := startHost(t, Spec{Argv: self("exit", "3"), Env: []string{}})
		waitFor(t, "the host's end", func() bool { return Gone(dir) && bytes.Contains(read(t, dir, HostLog), []byte(" ended\n")) })
		log := string(read(t, dir, HostLog))
		for _, want := range []string{" started, pid ", " the command ended: exit status 3\n", " ended\n"} {
			if !strings.Contains(log, want) {
				t.Errorf("host.log has no %q:\n%s", want, log)
			}
		}
	})
	t.Run("dropped connection and signal", func(t *testing.T) {
		dir := startHost(t, Spec{Argv: self("echo"), Env: []string{}})
		c, err := net.Dial("unix", SocketPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("{not a request\n"))
		waitFor(t, "the drop recorded", func() bool {
			return bytes.Contains(read(t, dir, HostLog), []byte("dropped a connection: reading a request: "))
		})
		c.Close()

		t.Run("signal", func(t *testing.T) {
			if signal.Ignored(syscall.SIGHUP) {
				t.Skip("SIGHUP is inherited as ignored; the host will not receive it")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(read(t, dir, HostPIDFile))))
			if err != nil {
				t.Fatal(err)
			}
			syscall.Kill(pid, syscall.SIGHUP) // the host this test started
			waitFor(t, "the signal recorded", func() bool { return bytes.Contains(read(t, dir, HostLog), []byte("ended by signal hangup")) })
			// Then it ends as it would have: nothing listens any more.
			waitFor(t, "the host's end", func() bool {
				c, err := net.Dial("unix", SocketPath(dir))
				if err == nil {
					c.Close()
				}
				return err != nil
			})
		})
	})
}
