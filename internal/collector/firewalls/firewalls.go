// Package firewalls collects the cloud firewalls of the account: what each one
// is attached to, how many rules it carries, and whether DigitalOcean has
// finished applying it.
//
// Two of the metrics are worth an alert rather than a dashboard.
// pending_changes counts the droplets a rule change has not reached yet; a
// firewall that sits there for more than a few minutes is not protecting what
// the ruleset says it protects. inbound_rules_open counts the inbound rules
// reachable from the whole internet, which is the number that should not grow
// without somebody deciding it should.
//
// This is configuration, not traffic. DigitalOcean reports no packet or
// connection counts for a firewall through its API, so nothing of that kind can
// be exported here.
package firewalls

import (
	"context"
	"log/slog"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// anywhereV4 and anywhereV6 are the source addresses that mean "the whole
// internet". A rule listing either is reachable from anywhere.
const (
	anywhereV4 = "0.0.0.0/0"
	anywhereV6 = "::/0"
)

// Metric descriptors.
var (
	infoDesc = prometheus.NewDesc("digitalocean_firewall_info",
		"Always 1. Its labels describe the firewall and how far DigitalOcean has applied it.",
		[]string{"id", "name", "status"}, nil)
	dropletsDesc = prometheus.NewDesc("digitalocean_firewall_droplets",
		"Number of droplets the firewall is attached to directly.",
		[]string{"id", "name"}, nil)
	tagsDesc = prometheus.NewDesc("digitalocean_firewall_tags",
		"Number of tags the firewall is attached to; droplets carrying one are covered too.",
		[]string{"id", "name"}, nil)
	inboundDesc = prometheus.NewDesc("digitalocean_firewall_inbound_rules",
		"Number of inbound rules in the firewall.",
		[]string{"id", "name"}, nil)
	outboundDesc = prometheus.NewDesc("digitalocean_firewall_outbound_rules",
		"Number of outbound rules in the firewall.",
		[]string{"id", "name"}, nil)
	openDesc = prometheus.NewDesc("digitalocean_firewall_inbound_rules_open",
		"Number of inbound rules whose sources include 0.0.0.0/0 or ::/0.",
		[]string{"id", "name"}, nil)
	pendingDesc = prometheus.NewDesc("digitalocean_firewall_pending_changes",
		"Number of droplets a change to the firewall has not been applied to yet.",
		[]string{"id", "name"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	infoDesc, dropletsDesc, tagsDesc, inboundDesc, outboundDesc, openDesc, pendingDesc,
}

// firewall is what one refresh learned about a single cloud firewall.
type firewall struct {
	id     string
	name   string
	status string

	droplets float64
	tags     float64
	inbound  float64
	outbound float64
	open     float64
	pending  float64
}

// Collector reports the cloud firewalls of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []firewall
}

// New returns a firewalls collector backed by client. The logger records what
// the scheduler never sees: a duplicate firewall dropped from a list that
// shifted between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "firewalls" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous firewalls in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	firewalls, err := paging.All(ctx, c.logger, "firewalls",
		func(f godo.Firewall) string { return f.ID }, c.client.Firewalls.List)
	if err != nil {
		return err
	}

	next := make([]firewall, 0, len(firewalls))
	for i := range firewalls {
		next = append(next, newFirewall(&firewalls[i]))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newFirewall converts one API firewall into its snapshot form.
func newFirewall(f *godo.Firewall) firewall {
	return firewall{
		id:       f.ID,
		name:     f.Name,
		status:   f.Status,
		droplets: float64(len(f.DropletIDs)),
		tags:     float64(len(f.Tags)),
		inbound:  float64(len(f.InboundRules)),
		outbound: float64(len(f.OutboundRules)),
		open:     float64(countOpen(f.InboundRules)),
		pending:  float64(len(f.PendingChanges)),
	}
}

// countOpen counts the inbound rules reachable from the whole internet. A rule
// with no sources at all reaches nothing and does not count; the API models an
// absent sources object as a nil pointer.
func countOpen(rules []godo.InboundRule) int {
	var open int
	for _, r := range rules {
		if r.Sources == nil {
			continue
		}
		for _, addr := range r.Sources.Addresses {
			if addr == anywhereV4 || addr == anywhereV6 {
				open++
				break
			}
		}
	}
	return open
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no firewalls, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, f := range snap {
		gauge(ch, infoDesc, 1, f.id, f.name, f.status)
		gauge(ch, dropletsDesc, f.droplets, f.id, f.name)
		gauge(ch, tagsDesc, f.tags, f.id, f.name)
		gauge(ch, inboundDesc, f.inbound, f.id, f.name)
		gauge(ch, outboundDesc, f.outbound, f.id, f.name)
		gauge(ch, openDesc, f.open, f.id, f.name)
		gauge(ch, pendingDesc, f.pending, f.id, f.name)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
