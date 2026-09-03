// Package dropletautoscale collects the droplet autoscale pools of the
// account: how each pool is allowed to scale, where it stands now, and the
// utilisation the scaling decisions are made from.
//
// A pool is configured one of two ways, and the metrics follow the split. A
// pool scaling on utilisation carries minimum and maximum instance counts and
// a target CPU or memory utilisation; a pool with a fixed target carries a
// single target instance count and nothing else. A metric that belongs to the
// other kind of configuration is absent rather than zero — a fixed-target pool
// reporting max_instances 0 would read as a pool forbidden to run anything.
package dropletautoscale

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// poolLabels are the labels every metric of the collector carries.
var poolLabels = []string{"id", "name"}

// Metric descriptors. The utilisation ratios are the API's own decimals
// between 0 and 1, passed through unchanged.
var (
	infoDesc = prometheus.NewDesc("digitalocean_droplet_autoscale_pool_info",
		"Always 1. Its labels describe where the pool creates droplets and the pool's status.",
		[]string{"id", "name", "region", "size", "status"}, nil)
	minDesc = prometheus.NewDesc("digitalocean_droplet_autoscale_pool_min_instances",
		"Minimum number of droplets the pool keeps. Absent for a pool with a fixed target.",
		poolLabels, nil)
	maxDesc = prometheus.NewDesc("digitalocean_droplet_autoscale_pool_max_instances",
		"Maximum number of droplets the pool may grow to. Absent for a pool with a fixed target.",
		poolLabels, nil)
	targetDesc = prometheus.NewDesc("digitalocean_droplet_autoscale_pool_target_instances",
		"Fixed number of droplets the pool is configured to run. "+
			"Absent for a pool that scales on utilisation.",
		poolLabels, nil)
	activeDesc = prometheus.NewDesc("digitalocean_droplet_autoscale_pool_active_instances",
		"Number of active droplets in the pool.",
		poolLabels, nil)
	targetCPUDesc = prometheus.NewDesc(
		"digitalocean_droplet_autoscale_pool_target_cpu_utilization_ratio",
		"Average CPU utilisation the pool scales towards, between 0 and 1. "+
			"Absent when the pool has no CPU target.",
		poolLabels, nil)
	targetMemoryDesc = prometheus.NewDesc(
		"digitalocean_droplet_autoscale_pool_target_memory_utilization_ratio",
		"Average memory utilisation the pool scales towards, between 0 and 1. "+
			"Absent when the pool has no memory target.",
		poolLabels, nil)
	currentCPUDesc = prometheus.NewDesc(
		"digitalocean_droplet_autoscale_pool_current_cpu_utilization_ratio",
		"Average CPU utilisation across the pool's droplets, between 0 and 1.",
		poolLabels, nil)
	currentMemoryDesc = prometheus.NewDesc(
		"digitalocean_droplet_autoscale_pool_current_memory_utilization_ratio",
		"Average memory utilisation across the pool's droplets, between 0 and 1.",
		poolLabels, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	infoDesc, minDesc, maxDesc, targetDesc, activeDesc,
	targetCPUDesc, targetMemoryDesc, currentCPUDesc, currentMemoryDesc,
}

// apiPool is one autoscale pool as the list endpoint reports it. It is decoded
// here rather than through godo's DropletAutoscalePool because that struct has
// no field for active_resources_count, which the API documents as required and
// this collector exists to report.
type apiPool struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	ActiveResources uint64 `json:"active_resources_count"`
	Config          struct {
		MinInstances            uint64  `json:"min_instances"`
		MaxInstances            uint64  `json:"max_instances"`
		TargetNumberInstances   uint64  `json:"target_number_instances"`
		TargetCPUUtilization    float64 `json:"target_cpu_utilization"`
		TargetMemoryUtilization float64 `json:"target_memory_utilization"`
	} `json:"config"`
	DropletTemplate struct {
		Region string `json:"region"`
		Size   string `json:"size"`
	} `json:"droplet_template"`
	CurrentUtilization *struct {
		CPU    float64 `json:"cpu"`
		Memory float64 `json:"memory"`
	} `json:"current_utilization"`
}

// poolsRoot is the body of one page of the pool list.
type poolsRoot struct {
	Pools []apiPool   `json:"autoscale_pools"`
	Links *godo.Links `json:"links"`
	Meta  *godo.Meta  `json:"meta"`
}

// Collector reports the droplet autoscale pools of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []apiPool
}

// New returns a droplet autoscale collector backed by client. The logger
// records what the scheduler never sees: a duplicate pool dropped from a list
// that shifted between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "dropletautoscale" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous pools in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	pools, err := paging.All(ctx, c.logger, "droplet autoscale pools",
		func(p apiPool) string { return p.ID }, c.listPools)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.snap = pools
	c.mu.Unlock()
	return nil
}

// listPools reads one page of the pool list. The request is issued through the
// godo client directly — same transport, same rate limiting and retries — and
// decoded into apiPool, because godo's own DropletAutoscale.List drops the
// active droplet count on the floor.
func (c *Collector) listPools(ctx context.Context, opts *godo.ListOptions) ([]apiPool, *godo.Response, error) {
	query := url.Values{}
	if opts.Page > 0 {
		query.Set("page", strconv.Itoa(opts.Page))
	}
	if opts.PerPage > 0 {
		query.Set("per_page", strconv.Itoa(opts.PerPage))
	}
	path := "v2/droplets/autoscale?" + query.Encode()

	req, err := c.client.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, err
	}
	root := new(poolsRoot)
	resp, err := c.client.Do(ctx, req, root)
	if err != nil {
		return nil, nil, err
	}
	// godo populates a response's paging links from the root struct of each of
	// its own services; a raw request has to do the same for paging.All to see
	// the last page.
	if root.Links != nil {
		resp.Links = root.Links
	}
	if root.Meta != nil {
		resp.Meta = root.Meta
	}
	return root.Pools, resp, nil
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no autoscale pools, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for i := range snap {
		collectPool(ch, &snap[i])
	}
}

// collectPool emits every metric one pool carries. The API's config is one of
// two shapes — bounds with utilisation targets, or a fixed instance count —
// and the fields of the shape not in use are absent from the response, so a
// zero here means "not this kind of pool" and emits nothing.
func collectPool(ch chan<- prometheus.Metric, p *apiPool) {
	gauge(ch, infoDesc, 1, p.ID, p.Name, p.DropletTemplate.Region, p.DropletTemplate.Size, p.Status)
	gauge(ch, activeDesc, float64(p.ActiveResources), p.ID, p.Name)

	if p.Config.MaxInstances > 0 {
		gauge(ch, minDesc, float64(p.Config.MinInstances), p.ID, p.Name)
		gauge(ch, maxDesc, float64(p.Config.MaxInstances), p.ID, p.Name)
	}
	if p.Config.TargetNumberInstances > 0 {
		gauge(ch, targetDesc, float64(p.Config.TargetNumberInstances), p.ID, p.Name)
	}
	if p.Config.TargetCPUUtilization > 0 {
		gauge(ch, targetCPUDesc, p.Config.TargetCPUUtilization, p.ID, p.Name)
	}
	if p.Config.TargetMemoryUtilization > 0 {
		gauge(ch, targetMemoryDesc, p.Config.TargetMemoryUtilization, p.ID, p.Name)
	}
	if u := p.CurrentUtilization; u != nil {
		gauge(ch, currentCPUDesc, u.CPU, p.ID, p.Name)
		gauge(ch, currentMemoryDesc, u.Memory, p.ID, p.Name)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
