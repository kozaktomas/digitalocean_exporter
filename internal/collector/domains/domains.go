// Package domains collects the DNS zones the account hosts.
//
// One list request per refresh covers the whole account, so this collector is
// cheap enough to leave enabled anywhere. What it can report is limited by what
// that request returns: a zone's name and its default TTL, and nothing about the
// records inside it. Counting records means a request per zone, which on an
// account with many domains would spend a large share of the hourly rate limit
// on data that changes rarely.
//
// The list response does carry each zone's full BIND zone file, so record counts
// could in principle be derived without extra requests. They are deliberately
// not: the file is a text format DigitalOcean documents no guarantees about, and
// a miscounted record is worse than an absent one.
//
// How many zones the account has is count(digitalocean_domain_ttl_seconds),
// which is also how the number of droplets or volumes is counted here.
package domains

import (
	"context"
	"fmt"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// domainsPerPage is how many domains one page request asks for, which is the
// most the API allows.
const domainsPerPage = 200

// ttlDesc is the only metric the collector emits.
var ttlDesc = prometheus.NewDesc("digitalocean_domain_ttl_seconds",
	"Default time-to-live of the DNS zone in seconds.",
	[]string{"domain"}, nil)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{ttlDesc}

// domain is what one refresh learned about a single DNS zone.
type domain struct {
	name string
	ttl  float64
}

// Collector reports the DNS zones of the account.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap []domain
}

// New returns a domains collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "domains" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous zones in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	opts := &godo.ListOptions{PerPage: domainsPerPage}
	var next []domain

	for {
		page, resp, err := c.client.Domains.List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list domains: %w", err)
		}
		for i := range page {
			next = append(next, domain{name: page[i].Name, ttl: float64(page[i].TTL)})
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return fmt.Errorf("next page of domains: %w", err)
		}
		opts.Page = current + 1
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account hosting no zones, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, d := range snap {
		ch <- prometheus.MustNewConstMetric(ttlDesc, prometheus.GaugeValue, d.ttl, d.name)
	}
}
