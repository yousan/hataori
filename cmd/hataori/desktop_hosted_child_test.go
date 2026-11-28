package main

import (
	"context"
	"os"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/surface"
)

// The production launcher invokes these hosted-only test executables exactly
// as it invokes the packaged daemon. This isolated dispatch keeps the existing
// shared TestMain and all other workers' fixtures unchanged. It never runs on
// an operator host or during ordinary tests and registers no real provider.
func init() {
	if os.Getenv("HATAORI_DESKTOP_HOSTED_FIXTURE") != "1" || len(os.Args) < 2 || os.Args[1] != "desktop" {
		return
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		os.Exit(2)
	}
	if agent.Register(fake.Name, fake.New) != nil || surface.Register(remoteSurface, remoteDriver) != nil {
		os.Exit(2)
	}
	os.Exit(desktopMain(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
}
