package dropletmetrics

import (
	"context"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// identity is the label set every metric here starts with. The series labels
// the monitoring API adds, if any, follow it.
var identity = []string{"id", "name"}

// filesystemLabels are the labels the API splits a filesystem series by. A
// droplet with several mounted filesystems reports one series per mount.
var filesystemLabels = []string{"device", "mountpoint", "fstype"}

// labelsWith returns the identity labels followed by extra, always in a slice
// of its own. Appending straight onto identity would be a trap: the moment it
// has spare capacity, two descriptors built that way share a backing array and
// the second silently overwrites the first one's labels.
func labelsWith(extra ...string) []string {
	labels := make([]string, 0, len(identity)+len(extra))
	labels = append(labels, identity...)
	return append(labels, extra...)
}

// Metric descriptors.
//
// digitalocean_droplet_memory_total_bytes is not the same figure as the
// droplets collector's digitalocean_droplet_memory_bytes. That one is the
// memory the droplet was sold, taken from its size; this one is what the
// operating system reports as installed, which is slightly less because the
// hypervisor and the kernel keep some.
var (
	cpuDesc = prometheus.NewDesc("digitalocean_droplet_cpu_seconds_total",
		"Cumulative CPU time of the droplet in seconds, by mode.",
		labelsWith("mode"), nil)
	memoryTotalDesc = prometheus.NewDesc("digitalocean_droplet_memory_total_bytes",
		"Total memory of the droplet as its operating system reports it.", identity, nil)
	memoryAvailableDesc = prometheus.NewDesc("digitalocean_droplet_memory_available_bytes",
		"Memory available for starting new applications without swapping.", identity, nil)
	memoryFreeDesc = prometheus.NewDesc("digitalocean_droplet_memory_free_bytes",
		"Memory not used for anything at all.", identity, nil)
	memoryCachedDesc = prometheus.NewDesc("digitalocean_droplet_memory_cached_bytes",
		"Memory used by the page cache.", identity, nil)
	filesystemSizeDesc = prometheus.NewDesc("digitalocean_droplet_filesystem_size_bytes",
		"Size of the filesystem.", labelsWith(filesystemLabels...), nil)
	filesystemFreeDesc = prometheus.NewDesc("digitalocean_droplet_filesystem_free_bytes",
		"Free space on the filesystem.", labelsWith(filesystemLabels...), nil)
	load1Desc = prometheus.NewDesc("digitalocean_droplet_load1",
		"Load average over the last minute.", identity, nil)
	load5Desc = prometheus.NewDesc("digitalocean_droplet_load5",
		"Load average over the last five minutes.", identity, nil)
	load15Desc = prometheus.NewDesc("digitalocean_droplet_load15",
		"Load average over the last fifteen minutes.", identity, nil)
	upDesc = prometheus.NewDesc("digitalocean_droplet_metrics_up",
		"Whether the droplet's last metrics fetch succeeded.", identity, nil)
	sampledDesc = prometheus.NewDesc("digitalocean_droplet_metrics_timestamp_seconds",
		"Unix time of the newest sample returned for the droplet.", identity, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	cpuDesc, memoryTotalDesc, memoryAvailableDesc, memoryFreeDesc, memoryCachedDesc,
	filesystemSizeDesc, filesystemFreeDesc, load1Desc, load5Desc, load15Desc,
	upDesc, sampledDesc,
}

// fetcher asks the monitoring API for one metric of one droplet.
type fetcher func(context.Context, *godo.Client, *godo.DropletMetricsRequest) (
	*godo.MetricsResponse, *godo.Response, error)

// spec describes one metric: where it comes from, which descriptor it feeds
// and which of the series' own labels are appended to the droplet's identity.
type spec struct {
	// name identifies the metric in error messages.
	name string
	// desc is the descriptor the samples are emitted against.
	desc *prometheus.Desc
	// valueType is a counter only for cumulative CPU time; everything else
	// here is a current reading.
	valueType prometheus.ValueType
	// seriesLabels are read off each returned series, in order, and appended
	// to the droplet's id and name.
	seriesLabels []string
	// fetch performs the request.
	fetch fetcher
}

// specs is every metric fetched per droplet, and so also the request cost of
// one refresh: len(specs) requests for each droplet in the account.
//
// Bandwidth is deliberately absent. The API splits it by interface and
// direction and takes one request per combination, which would add four
// requests per droplet — more than a third of the budget — for a figure the
// account's own bill already summarises.
var specs = []spec{
	{
		name: "cpu", desc: cpuDesc, valueType: prometheus.CounterValue,
		seriesLabels: []string{"mode"},
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletCPU(ctx, r)
		},
	},
	{
		name: "memory_total", desc: memoryTotalDesc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletTotalMemory(ctx, r)
		},
	},
	{
		name: "memory_available", desc: memoryAvailableDesc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletAvailableMemory(ctx, r)
		},
	},
	{
		name: "memory_free", desc: memoryFreeDesc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletFreeMemory(ctx, r)
		},
	},
	{
		name: "memory_cached", desc: memoryCachedDesc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletCachedMemory(ctx, r)
		},
	},
	{
		name: "filesystem_size", desc: filesystemSizeDesc, valueType: prometheus.GaugeValue,
		seriesLabels: filesystemLabels,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletFilesystemSize(ctx, r)
		},
	},
	{
		name: "filesystem_free", desc: filesystemFreeDesc, valueType: prometheus.GaugeValue,
		seriesLabels: filesystemLabels,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletFilesystemFree(ctx, r)
		},
	},
	{
		name: "load_1", desc: load1Desc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletLoad1(ctx, r)
		},
	},
	{
		name: "load_5", desc: load5Desc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletLoad5(ctx, r)
		},
	},
	{
		name: "load_15", desc: load15Desc, valueType: prometheus.GaugeValue,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.DropletMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetDropletLoad15(ctx, r)
		},
	},
}
