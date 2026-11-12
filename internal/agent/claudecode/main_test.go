package claudecode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/yousan/hataori/internal/host"
)

// The test binary is also the session host, the hook command and an idle
// agent: `<test binary> role <name> args...`.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "role" {
		switch os.Args[2] {
		case "host":
			if host.Run(os.Args[3]) != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "hook":
			os.Exit(host.HookMain(os.Args[3:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
		case "login-pure":
			os.Exit(loginPure())
		case "idle":
			os.Stdin.Read(make([]byte, 1))
			os.Exit(0)
		}
		os.Exit(99)
	}
	os.Exit(m.Run())
}

// killGroups kills the host and the command started in stateDir, and only
// those.
func killGroups(stateDir string) {
	for _, f := range []string{host.PIDFile, host.HostPIDFile} {
		if b, err := os.ReadFile(filepath.Join(stateDir, f)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}
}
