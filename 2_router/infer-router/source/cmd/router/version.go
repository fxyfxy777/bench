// Package main defines build-time version information.
// These variables are set via ldflags during build.
package main

import (
	"github.com/yzx/rl-router/pkg/version"
)

var (
	// Version is the current version, set via -ldflags.
	// Defaults to git describe --tags --always (e.g., v1.0.0 or a991a8f).
	Version = "unknown"
	// Commit is the git commit hash, set via -ldflags.
	Commit = "unknown"
	// BuildDate is the build timestamp, set via -ldflags.
	BuildDate = "unknown"
)

func init() {
	// Set build info to the version package for use across the application.
	version.Set(Version, Commit, BuildDate)
	// Register Prometheus metrics.
	version.RegisterMetrics(Version, Commit, BuildDate)
}
