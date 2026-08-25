// Package limits counts what the account's resource limits cap: droplets,
// reserved IP addresses and volumes.
//
// The account collector reports the limits themselves. Paired with the counts
// here they answer the question a limit only raises — how much of it is left —
// as digitalocean_account_droplets / digitalocean_account_droplet_limit.
package limits

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// countPerPage is the page size every count request asks for. The figures come
// from meta.total, so one item is all that needs to travel: downloading the
// whole inventory every few minutes to arrive at three numbers would cost far
// more than it tells.
const countPerPage = 1

// ErrNoTotal reports that a list response carried no meta.total to count.
var ErrNoTotal = errors.New("response has no meta.total")

// Metric descriptors.
var (
	dropletsDesc = prometheus.NewDesc("digitalocean_account_droplets",
		"Number of droplets on the account.", nil, nil)
	reservedIPsDesc = prometheus.NewDesc("digitalocean_account_reserved_ips",
		"Number of reserved IP addresses on the account.", nil, nil)
	volumesDesc = prometheus.NewDesc("digitalocean_account_volumes",
		"Number of block storage volumes on the account.", nil, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{dropletsDesc, reservedIPsDesc, volumesDesc}

// snapshot is an immutable set of counts from one successful refresh.
type snapshot struct {
	droplets    float64
	reservedIPs float64
	volumes     float64
}

// Collector reports how many of the limited resources the account uses.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap *snapshot
}

// New returns a limits collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "limits" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. All three counts are read before the
// snapshot is replaced, so one failing endpoint leaves the previous counts
// untouched rather than mixing old and new figures.
func (c *Collector) Refresh(ctx context.Context) error {
	droplets, err := c.countDroplets(ctx)
	if err != nil {
		return err
	}
	reservedIPs, err := c.countReservedIPs(ctx)
	if err != nil {
		return err
	}
	volumes, err := c.countVolumes(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.snap = &snapshot{droplets: droplets, reservedIPs: reservedIPs, volumes: volumes}
	c.mu.Unlock()
	return nil
}

// countDroplets returns how many droplets the account has.
func (c *Collector) countDroplets(ctx context.Context) (float64, error) {
	_, resp, err := c.client.Droplets.List(ctx, &godo.ListOptions{PerPage: countPerPage})
	if err != nil {
		return 0, fmt.Errorf("list droplets: %w", err)
	}
	return total(resp, "droplets")
}

// countReservedIPs returns how many reserved IP addresses the account has.
func (c *Collector) countReservedIPs(ctx context.Context) (float64, error) {
	_, resp, err := c.client.ReservedIPs.List(ctx, &godo.ListOptions{PerPage: countPerPage})
	if err != nil {
		return 0, fmt.Errorf("list reserved IPs: %w", err)
	}
	return total(resp, "reserved IPs")
}

// countVolumes returns how many block storage volumes the account has.
func (c *Collector) countVolumes(ctx context.Context) (float64, error) {
	params := &godo.ListVolumeParams{ListOptions: &godo.ListOptions{PerPage: countPerPage}}
	_, resp, err := c.client.Storage.ListVolumes(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("list volumes: %w", err)
	}
	return total(resp, "volumes")
}

// total reads the count out of a list response. A response without meta.total
// fails the refresh: the page holds a single item by design, so falling back to
// its length would report one droplet for an account running a hundred.
func total(resp *godo.Response, resource string) (float64, error) {
	if resp == nil || resp.Meta == nil {
		return 0, fmt.Errorf("count %s: %w", resource, ErrNoTotal)
	}
	return float64(resp.Meta.Total), nil
}

// Collect implements collector.Collector. Before the first successful refresh
// it emits nothing rather than zeros, which would read as an empty account.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	if snap == nil {
		return
	}
	for _, m := range []struct {
		desc  *prometheus.Desc
		value float64
	}{
		{dropletsDesc, snap.droplets},
		{reservedIPsDesc, snap.reservedIPs},
		{volumesDesc, snap.volumes},
	} {
		ch <- prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, m.value)
	}
}
