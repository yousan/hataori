// Command hataori runs coding agents in parallel from chat.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case len(args) == 2 && args[0] == "host":
		// Started by the daemon through host.Start; its output goes nowhere.
		if err := host.Run(args[1]); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case len(args) >= 1 && args[0] == "hook":
		return host.HookMain(args[1:], os.Stdin, stdout, stderr, os.Getenv)
	}
	fmt.Fprintln(stderr, "usage: hataori version | hataori host <state-dir> | hataori hook [--wait] [--timeout DURATION]")
	return 2
}
