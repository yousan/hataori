// Command hataori-desktop-transport is a bundled private-pipe transport, not a
// user service. Only Electron's main process starts it, once per connection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/desktopbridge"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/version"
)

func main() {
	fs := flag.NewFlagSet("desktop transport", flag.ContinueOnError)
	root := fs.String("root", "", "the explicitly selected private root")
	instance := fs.String("instance", "", "the pinned installation identity")
	inspect := fs.Bool("inspect", false, "establish the selected root's first binding")
	if fs.Parse(os.Args[1:]) != nil || fs.NArg() != 0 || *root == "" || !*inspect && *instance == "" {
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	if *inspect {
		ctx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		var cfg nativehttp.Config
		cfg, err = nativehttp.Inspect(ctx, *root, version.Version)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(cfg)
		}
	} else {
		err = desktopbridge.Relay(ctx, nativehttp.Config{Root: *root, Instance: *instance, Version: version.Version}, os.Stdin, os.Stdout)
	}
	if err != nil {
		// Never copy HTTP bodies, paths, credentials or terminal contents into
		// transport diagnostics. Main reports the selected instance separately.
		fmt.Fprintln(os.Stderr, "Private desktop transport unavailable")
		os.Exit(1)
	}
}
