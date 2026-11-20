package host

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A shutdown sends SIGTERM to every process at once, here the command's
// first: the host records the end as a shutdown's. A
// command killed on its own while its host runs is recorded as it ended,
// with no shutdown.
func TestShutdownIsRecordedAndACrashIsNot(t *testing.T) {
	for _, c := range []struct {
		name     string
		shutdown bool
	}{{"shutdown", true}, {"crash", false}} {
		t.Run(c.name, func(t *testing.T) {
			dir := startHost(t, Spec{Argv: self("echo"), Env: []string{}, Terminal: true})
			pid := func(f string) int {
				t.Helper()
				n, err := strconv.Atoi(strings.TrimSpace(string(read(t, dir, f))))
				if err != nil || n <= 1 {
					t.Fatalf("%s: %q", f, read(t, dir, f))
				}
				return n
			}
			// The host listens before it starts the command.
			waitFor(t, "the command's pid", func() bool { return len(strings.TrimSpace(string(read(t, dir, PIDFile)))) > 0 })
			command, host := pid(PIDFile), pid(HostPIDFile)
			if c.shutdown {
				syscall.Kill(-command, syscall.SIGTERM)
				syscall.Kill(host, syscall.SIGTERM)
			} else {
				syscall.Kill(-command, syscall.SIGKILL)
			}
			waitGoneT(t, dir)
			st, ok, err := ReadStatus(dir)
			if !ok || err != nil {
				log, _ := os.ReadFile(filepath.Join(dir, HostLog))
				t.Fatalf("no status: %v; the host's log:\n%s", err, log)
			}
			if st.Shutdown != c.shutdown || !st.Exited || st.Stopped {
				t.Fatalf("status %+v, want shutdown %v", st, c.shutdown)
			}
		})
	}
}
