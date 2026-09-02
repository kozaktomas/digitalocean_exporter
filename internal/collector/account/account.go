// Package account collects DigitalOcean account status and limit metrics.
//
// Billing figures are not collected here: they need a token with the billing
// scope and live in the balance collector, so an account whose token cannot
// read billing still gets these metrics.
package account

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// activeStatus is the only account status that is not a problem.
const activeStatus = "active"

// knownStatuses are the account statuses DigitalOcean documents. All three are
// reported on every scrape, so that a dashboard or an alert has a series for
// the status it is looking for before the account ever enters it: a status
// that only appears once the account is in trouble is a query that returns no
// data exactly when it matters.
var knownStatuses = []string{activeStatus, "warning", "locked"}

// Metric descriptors.
var (
	activeDesc = prometheus.NewDesc("digitalocean_account_active",
		"Whether the account status is active.", nil, nil)
	statusDesc = prometheus.NewDesc("digitalocean_account_status",
		"Always 1 for the account's current status and 0 for every other known one.",
		[]string{"status"}, nil)
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
	status          string
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
		activeDesc, statusDesc, verifiedDesc, dropletLimitDesc, floatingIPLimitDesc,
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
		status:          acct.Status,
		active:          boolToFloat(acct.Status == activeStatus),
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

	c.collectStatus(ch, snap.status)

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

// collectStatus emits one series per known status, and one more for a status
// DigitalOcean has invented since this was written: an unknown status left out
// would make every series read 0, which is indistinguishable from the exporter
// having stopped reporting the metric.
func (c *Collector) collectStatus(ch chan<- prometheus.Metric, status string) {
	statuses := knownStatuses
	if status != "" && !slices.Contains(statuses, status) {
		statuses = append(slices.Clone(statuses), status)
	}
	for _, s := range statuses {
		ch <- prometheus.MustNewConstMetric(statusDesc, prometheus.GaugeValue,
			boolToFloat(s == status), s)
	}
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
