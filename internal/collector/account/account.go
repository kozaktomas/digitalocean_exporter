// Package account collects DigitalOcean account and balance metrics.
package account

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// Metric descriptors.
//
// The month-to-date and balance metric names deliberately omit the "account_"
// infix: they match the names used by metalmatze/digitalocean_exporter, so
// dashboards survive a migration from it unchanged.
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
	balanceDesc = prometheus.NewDesc("digitalocean_account_balance",
		"Current account balance in the account currency.", nil, nil)
	monthToDateBalanceDesc = prometheus.NewDesc("digitalocean_month_to_date_balance",
		"Month-to-date balance in the account currency.", nil, nil)
	monthToDateUsageDesc = prometheus.NewDesc("digitalocean_month_to_date_usage",
		"Month-to-date usage in the account currency.", nil, nil)
	generatedAtDesc = prometheus.NewDesc("digitalocean_balance_generated_at",
		"Unix timestamp the balance figures were generated at.", nil, nil)
)

// snapshot is an immutable set of values from one successful refresh.
type snapshot struct {
	active             float64
	verified           float64
	dropletLimit       float64
	floatingIPLimit    float64
	reservedIPLimit    float64
	volumeLimit        float64
	balance            float64
	monthToDateBalance float64
	monthToDateUsage   float64
	generatedAt        float64
}

// Collector reports account limits and billing figures.
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
		reservedIPLimitDesc, volumeLimitDesc, balanceDesc,
		monthToDateBalanceDesc, monthToDateUsageDesc, generatedAtDesc,
	} {
		ch <- d
	}
}

// Refresh implements collector.Collector. The snapshot is replaced only once
// every value has been fetched and parsed, so a partial failure leaves the
// previous snapshot intact.
func (c *Collector) Refresh(ctx context.Context) error {
	acct, _, err := c.client.Account.Get(ctx)
	if err != nil {
		return fmt.Errorf("get account: %w", err)
	}

	bal, _, err := c.client.Balance.Get(ctx)
	if err != nil {
		return fmt.Errorf("get balance: %w", err)
	}

	balance, err := parseAmount(bal.AccountBalance, "account_balance")
	if err != nil {
		return err
	}
	monthToDateBalance, err := parseAmount(bal.MonthToDateBalance, "month_to_date_balance")
	if err != nil {
		return err
	}
	monthToDateUsage, err := parseAmount(bal.MonthToDateUsage, "month_to_date_usage")
	if err != nil {
		return err
	}

	next := &snapshot{
		active:             boolToFloat(acct.Status == "active"),
		verified:           boolToFloat(acct.EmailVerified),
		dropletLimit:       float64(acct.DropletLimit),
		floatingIPLimit:    float64(acct.FloatingIPLimit),
		reservedIPLimit:    float64(acct.ReservedIPLimit),
		volumeLimit:        float64(acct.VolumeLimit),
		balance:            balance,
		monthToDateBalance: monthToDateBalance,
		monthToDateUsage:   monthToDateUsage,
		generatedAt:        float64(bal.GeneratedAt.Unix()),
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// Collect implements collector.Collector. Before the first successful refresh
// it emits nothing rather than zeros, so a starting exporter cannot be read as
// an account with no droplets and no money.
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
		{balanceDesc, snap.balance},
		{monthToDateBalanceDesc, snap.monthToDateBalance},
		{monthToDateUsageDesc, snap.monthToDateUsage},
		{generatedAtDesc, snap.generatedAt},
	} {
		ch <- prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, m.value)
	}
}

// parseAmount converts a DigitalOcean money string such as "23.44" to a float.
// A parse failure is an error, never a silent zero: zero is a legitimate
// balance, and conflating the two would break the billing metrics in silence.
func parseAmount(raw, field string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", field, raw, err)
	}
	return value, nil
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
