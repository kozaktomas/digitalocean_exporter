// Package collector defines the collector contract and the scheduler that
// keeps collector snapshots fresh independently of Prometheus scrapes.
package collector

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector gathers one family of DigitalOcean metrics on its own schedule.
//
// Refresh performs the I/O and replaces an internal snapshot; Collect only
// reads that snapshot. Keeping the two apart is what stops a slow API call
// from blocking or failing a Prometheus scrape.
type Collector interface {
	// Name identifies the collector in flags and self-metrics.
	Name() string
	// Describe sends the descriptors of every metric the collector can emit.
	Describe(ch chan<- *prometheus.Desc)
	// Refresh fetches fresh data and replaces the snapshot. On error the
	// previous snapshot must survive untouched.
	Refresh(ctx context.Context) error
	// Collect reads the current snapshot. It must never perform I/O.
	Collect(ch chan<- prometheus.Metric)
}
