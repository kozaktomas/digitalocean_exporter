// Package version exposes the build metadata stamped into the binary at link
// time and publishes it as a Prometheus metric.
package version

import (
	"fmt"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
)

// Version is the released version, overridden at link time.
var Version = "dev"

// Commit is the short commit the binary was built from, overridden at link time.
var Commit = "none"

// String returns the single line printed by --version: the binary name, the
// release it was built as, the commit it came from and the Go toolchain that
// built it. Version and Commit read "dev" and "none" in a build that was not
// stamped at link time, which is what a plain `go build` produces.
func String() string {
	return fmt.Sprintf("digitalocean_exporter, version %s (commit %s, %s)",
		Version, Commit, runtime.Version())
}

// buildInfoDesc describes the build metadata metric.
var buildInfoDesc = prometheus.NewDesc(
	"digitalocean_exporter_build_info",
	"Build metadata of the running exporter. Always 1.",
	[]string{"version", "commit", "goversion"},
	nil,
)

// Collector publishes the exporter's build metadata.
type Collector struct{}

// NewCollector returns a Prometheus collector for the build metadata.
func NewCollector() *Collector {
	return &Collector{}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- buildInfoDesc
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		buildInfoDesc, prometheus.GaugeValue, 1, Version, Commit, runtime.Version(),
	)
}
