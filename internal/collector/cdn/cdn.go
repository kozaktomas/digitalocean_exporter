// Package cdn collects the CDN endpoints of the account: what each one caches
// for and which certificate it serves.
//
// This is inventory, not traffic. DigitalOcean exposes no request count, no
// bandwidth and no hit ratio for a CDN endpoint through its API, so nothing of
// that kind can be reported here; what an endpoint fronts is a Spaces bucket,
// whose size the spaces collector measures.
//
// certificate_id is carried on the info metric so an endpoint can be joined to
// the certificate it serves.
package cdn

import (
	"context"
	"fmt"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// endpointsPerPage is how many endpoints one page request asks for, which is
// the most the API allows.
const endpointsPerPage = 200

// Metric descriptors.
var (
	ttlDesc = prometheus.NewDesc("digitalocean_cdn_endpoint_ttl_seconds",
		"Cache time-to-live of the CDN endpoint in seconds.",
		[]string{"id", "origin", "endpoint"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_cdn_endpoint_info",
		"Always 1. Its labels describe the endpoint's custom domain and certificate.",
		[]string{"id", "origin", "endpoint", "custom_domain", "certificate_id"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{ttlDesc, infoDesc}

// endpoint is what one refresh learned about a single CDN endpoint.
type endpoint struct {
	id            string
	origin        string
	endpoint      string
	customDomain  string
	certificateID string

	ttl float64
}

// Collector reports the CDN endpoints of the account.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap []endpoint
}

// New returns a CDN collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "cdn" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous endpoints in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	opts := &godo.ListOptions{PerPage: endpointsPerPage}
	var next []endpoint

	for {
		page, resp, err := c.client.CDNs.List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list CDN endpoints: %w", err)
		}
		for i := range page {
			next = append(next, newEndpoint(&page[i]))
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return fmt.Errorf("next page of CDN endpoints: %w", err)
		}
		opts.Page = current + 1
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newEndpoint converts one API CDN endpoint into its snapshot form.
func newEndpoint(e *godo.CDN) endpoint {
	return endpoint{
		id:            e.ID,
		origin:        e.Origin,
		endpoint:      e.Endpoint,
		customDomain:  e.CustomDomain,
		certificateID: e.CertificateID,
		ttl:           float64(e.TTL),
	}
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no CDN endpoints, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, e := range snap {
		gauge(ch, ttlDesc, e.ttl, e.id, e.origin, e.endpoint)
		gauge(ch, infoDesc, 1, e.id, e.origin, e.endpoint, e.customDomain, e.certificateID)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
