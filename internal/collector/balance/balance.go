// Package balance collects DigitalOcean billing metrics.
//
// Billing lives apart from the account collector on purpose. Reading
// /v2/customers/my/balance needs a token with the billing scope, which a token
// scoped to resources alone does not have; keeping the two collectors separate
// means such a token still exports every account metric, and only this
// collector reports a failure.
package balance

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
// These names deliberately omit the "account_" infix: they match the names
// used by metalmatze/digitalocean_exporter, so dashboards survive a migration
// from it unchanged.
var (
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
	balance            float64
	monthToDateBalance float64
	monthToDateUsage   float64
	generatedAt        float64
}

// Collector reports the billing figures of the account.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap *snapshot
}

// New returns a balance collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "balance" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		balanceDesc, monthToDateBalanceDesc, monthToDateUsageDesc, generatedAtDesc,
	} {
		ch <- d
	}
}

// Refresh implements collector.Collector. The snapshot is replaced only once
// every value has been fetched and parsed, so a partial failure leaves the
// previous snapshot intact.
func (c *Collector) Refresh(ctx context.Context) error {
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
// an account with no money.
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
