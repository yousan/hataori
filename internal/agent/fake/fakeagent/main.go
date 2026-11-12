// Command fakeagent is the fake agent's program, for tests run by hand.
package main

import (
	"os"

	"github.com/yousan/hataori/internal/agent/fake"
)

func main() { os.Exit(fake.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
