// Package account collects DigitalOcean account status and limit metrics.
//
// Billing figures are not collected here: they need a token with the billing
// scope and live in the balance collector, so an account whose token cannot
// read billing still gets these metrics.
package account

import (
	"context"
	"fmt"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// Metric descriptors.
var (
	activeDesc = prometheus.NewDesc("digitalocean_account_active",
		"Whether the account status is active.", nil, nil)
	verifiedDesc = prometheus.NewDesc("digitalocean_account_verified",
		"Whether the account email address is verified.", nil, nil)
	dropletLimitDesc = prometheus.NewDesc("digitalocean_account_droplet_limit",
		"Maximum number of droplets the account may have.", nil, nil)
	floatingIPLimitDesc = prometheus.NewDesc("digitalocean_account_floating_ip_limit",
		"Maximum number of floating IPs the account may have.", nil, nil)
	reservedIPLimitDesc = prometheus.NewDesc("digitalocean_account_reserved_ip_limit",
		"Maximum number of reserved IPs the account may have.", nil, nil)
	volumeLimitDesc = prometheus.NewDesc("digitalocean_account_volume_limit",
		"Maximum number of volumes the account may have.", nil, nil)
)

// snapshot is an immutable set of values from one successful refresh.
type snapshot struct {
	active          float64
	verified        float64
	dropletLimit    float64
	floatingIPLimit float64
	reservedIPLimit float64
	volumeLimit     float64
}

// Collector reports account status and resource limits.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap *snapshot
}

// New returns an account collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "account" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		activeDesc, verifiedDesc, dropletLimitDesc, floatingIPLimitDesc,
		reservedIPLimitDesc, volumeLimitDesc,
	} {
		ch <- d
	}
}

// Refresh implements collector.Collector. The snapshot is replaced only once
// every value has been fetched, so a failure leaves the previous snapshot
// intact.
func (c *Collector) Refresh(ctx context.Context) error {
	acct, _, err := c.client.Account.Get(ctx)
	if err != nil {
		return fmt.Errorf("get account: %w", err)
	}

	next := &snapshot{
		active:          boolToFloat(acct.Status == "active"),
		verified:        boolToFloat(acct.EmailVerified),
		dropletLimit:    float64(acct.DropletLimit),
		floatingIPLimit: float64(acct.FloatingIPLimit),
		reservedIPLimit: float64(acct.ReservedIPLimit),
		volumeLimit:     float64(acct.VolumeLimit),
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// Collect implements collector.Collector. Before the first successful refresh
// it emits nothing rather than zeros, so a starting exporter cannot be read as
// an account with no droplets.
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
		{activeDesc, snap.active},
		{verifiedDesc, snap.verified},
		{dropletLimitDesc, snap.dropletLimit},
		{floatingIPLimitDesc, snap.floatingIPLimit},
		{reservedIPLimitDesc, snap.reservedIPLimit},
		{volumeLimitDesc, snap.volumeLimit},
	} {
		ch <- prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, m.value)
	}
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
