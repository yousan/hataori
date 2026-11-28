// ci-coordinator is built from a reviewed control revision and invoked outside
// candidate checkouts. It never installs or activates itself.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/yousan/hataori/cmd/internal/cipolicy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	binary, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(cipolicy.RunCLI(ctx, os.Args[1:], os.Stdout, binary))
}
