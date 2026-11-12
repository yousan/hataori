// Command hataori runs coding agents in parallel from chat.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/yousan/hataori/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	fmt.Fprintln(stderr, "usage: hataori version")
	return 2
}
