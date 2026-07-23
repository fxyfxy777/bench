// Package version provides build information for the router.
package version

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// buildInfo is the current build information, set during init().
	buildInfo BuildInfo
	once      sync.Once
)

// BuildInfo contains version information about the build.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

// Set sets the build information. This is called during init() from cmd/router/version.go.
func Set(version, commit, buildDate string) {
	once.Do(func() {
		buildInfo = BuildInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
		}
	})
}

// Get returns the current build information.
func Get() BuildInfo {
	return buildInfo
}

// RegisterMetrics registers the build_info metric with Prometheus.
// This should be called from cmd/router/version.go init().
func RegisterMetrics(version, commit, buildDate string) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "rl_router",
			Name:      "build_info",
			Help:      "Build information, value is always 1, use labels to identify version.",
			ConstLabels: prometheus.Labels{
				"version":    version,
				"commit":     commit,
				"build_date": buildDate,
			},
		},
		func() float64 { return 1 },
	))
}