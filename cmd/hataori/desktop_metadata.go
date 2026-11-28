package main

import (
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/version"
	"github.com/yousan/hataori/internal/wire"
)

// Metadata comes from the same binary being bundled, without opening an
// instance, reading configuration, starting an agent or acquiring a service.
func desktopMetadata() map[string]any {
	return map[string]any{"version": version.Version, "commit": version.ReadBuild().Commit,
		"schema": store.SchemaVersion(), "control_protocol": wire.Version, "host_capability": host.InputBatchVersion}
}
