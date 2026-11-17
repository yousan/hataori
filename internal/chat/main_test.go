package chat

import (
	"os"
	"testing"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/host"
)

// The test binary is also `hataori host`, `hataori hook` and the fake agent,
// for the tests that run sessions.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(host.HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 2 && os.Args[1] == "role" {
		switch os.Args[2] {
		case "host":
			if host.Run(os.Args[3]) != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "fake":
			os.Exit(fake.Main(os.Args[3:], os.Stdin, os.Stdout, os.Stderr))
		}
		os.Exit(99)
	}
	os.Exit(m.Run())
}
