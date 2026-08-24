// Package version exposes the build metadata stamped into the binary at link
// time and publishes it as a Prometheus metric.
package version

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
)

// Version is the released version, overridden at link time.
var Version = "dev"

// Commit is the short commit the binary was built from, overridden at link time.
var Commit = "none"

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
