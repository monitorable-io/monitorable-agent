// Package version exposes the collector build version, injected at link time via
// -ldflags "-X github.com/monitorable-io/monitorable-agent/internal/version.Version=vX.Y.Z".
package version

// Version is the build version. "dev" for local/untagged builds.
var Version = "dev"
