// Package certificates collects the TLS certificates the account holds for its
// load balancers and CDN endpoints.
//
// The metric worth alerting on is the expiry timestamp. DigitalOcean renews a
// lets_encrypt certificate on its own, but renewal can fail quietly — the
// certificate keeps its old not_after and its state turns to error — so an
// alert on expiry rather than on state is what catches it. A custom
// certificate is never renewed by anyone but its owner.
//
// The id label matches the certificate_id carried on
// digitalocean_cdn_endpoint_info, so an endpoint can be joined to the expiry of
// the certificate it serves.
package certificates

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// certificatesPerPage is how many certificates one page request asks for,
// which is the most the API allows.
const certificatesPerPage = 200

// Metric descriptors.
var (
	expiryDesc = prometheus.NewDesc("digitalocean_certificate_expiry_timestamp_seconds",
		"Expiry of the certificate as a Unix timestamp.",
		[]string{"id", "name", "type"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_certificate_info",
		"Always 1. Its labels describe the certificate's type, state and fingerprint.",
		[]string{"id", "name", "type", "state", "sha1_fingerprint"}, nil)
	dnsNamesDesc = prometheus.NewDesc("digitalocean_certificate_dns_names",
		"Number of DNS names the certificate covers.",
		[]string{"id", "name"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{expiryDesc, infoDesc, dnsNamesDesc}

// certificate is what one refresh learned about a single certificate.
type certificate struct {
	id          string
	name        string
	kind        string
	state       string
	fingerprint string

	dnsNames float64
	// expires reports whether notAfter holds a usable timestamp. A certificate
	// whose not_after the API omits or spells in some other format keeps its
	// other metrics and simply has no expiry one, rather than claiming to have
	// expired at the epoch.
	expires  bool
	notAfter float64
}

// Collector reports the TLS certificates of the account.
type Collector struct {
	client *godo.Client

	mu   sync.RWMutex
	snap []certificate
}

// New returns a certificates collector backed by client.
func New(client *godo.Client) *Collector {
	return &Collector{client: client}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "certificates" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous certificates in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	opts := &godo.ListOptions{PerPage: certificatesPerPage}
	var next []certificate

	for {
		page, resp, err := c.client.Certificates.List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list certificates: %w", err)
		}
		for i := range page {
			next = append(next, newCertificate(&page[i]))
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return fmt.Errorf("next page of certificates: %w", err)
		}
		opts.Page = current + 1
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newCertificate converts one API certificate into its snapshot form.
func newCertificate(cert *godo.Certificate) certificate {
	out := certificate{
		id:          cert.ID,
		name:        cert.Name,
		kind:        cert.Type,
		state:       cert.State,
		fingerprint: cert.SHA1Fingerprint,
		dnsNames:    float64(len(cert.DNSNames)),
	}
	if notAfter, err := time.Parse(time.RFC3339, cert.NotAfter); err == nil {
		out.expires = true
		out.notAfter = float64(notAfter.Unix())
	}
	return out
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account holding no certificates, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, cert := range snap {
		gauge(ch, infoDesc, 1, cert.id, cert.name, cert.kind, cert.state, cert.fingerprint)
		gauge(ch, dnsNamesDesc, cert.dnsNames, cert.id, cert.name)
		if cert.expires {
			gauge(ch, expiryDesc, cert.notAfter, cert.id, cert.name, cert.kind)
		}
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
