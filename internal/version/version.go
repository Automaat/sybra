// Package version holds the build-time version string.
// Set via ldflags: -X github.com/Automaat/synapse/internal/version.Version=v1.2.3
package version

// Version is the application version, injected at build time.
// Falls back to "dev" when not set.
var Version = "dev"
