// Package version reports which build of hataori is running.
package version

import "runtime/debug"

// Version is set at build time with
// -ldflags "-X github.com/yousan/hataori/internal/version.Version=<version>".
var Version = "dev"

// String returns hataori's name and version.
func String() string {
	return "hataori " + Version
}

// Build is what the Go toolchain recorded of the source a binary was built
// from (its build info, vcs.revision and vcs.modified).
type Build struct {
	Commit   string // the commit it was built from; empty when the build info has none
	Modified bool   // the source had changes not in that commit
}

// ReadBuild returns the running binary's Build. A test may replace it.
var ReadBuild = func() Build {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Build{}
	}
	var b Build
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}

// Line returns the line printed by `hataori version`: String, and the
// commit of the build, "modified" when the source had changes not in it.
func Line() string {
	b := ReadBuild()
	switch {
	case b.Commit == "":
		return String() + " (no commit in its build info)"
	case b.Modified:
		return String() + " (commit " + b.Commit + ", modified)"
	}
	return String() + " (commit " + b.Commit + ")"
}
