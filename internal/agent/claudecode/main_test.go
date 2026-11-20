package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/proctest"
)

// The test binary is also the session host, the hook command, the status
// line command and an idle agent: `<test binary> role <name> args...`.
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
		case "windowhook":
			os.Exit(WindowHookMain(os.Args[3:], os.Stdin, os.Stderr, os.Getenv))
		case "statusline":
			os.Exit(StatusLineMain(os.Args[3:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
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
// those: a pid file left by one that ended may name another process by now.
func killGroups(stateDir string) {
	for _, f := range []string{host.PIDFile, host.HostPIDFile} {
		proctest.Kill(filepath.Join(stateDir, f))
	}
}
