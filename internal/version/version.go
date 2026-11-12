// Package version reports which build of hataori is running.
package version

// Version is set at build time with
// -ldflags "-X github.com/yousan/hataori/internal/version.Version=<version>".
var Version = "dev"

// String returns the line printed by `hataori version`.
func String() string {
	return "hataori " + Version
}
