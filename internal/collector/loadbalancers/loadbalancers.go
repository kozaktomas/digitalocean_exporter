// Package loadbalancers collects the load balancers of the account: whether
// each one is active, how many droplets it proxies to and what it costs.
//
// The metric prefix is digitalocean_loadbalancer_, without the underscore the
// rest of the exporter would suggest. It is the prefix of the older, widely
// deployed DigitalOcean exporter, and matching it is worth more than internal
// consistency: digitalocean_loadbalancer_status and
// digitalocean_loadbalancer_droplets carry that exporter's names and label
// sets exactly, so dashboards survive a migration.
//
// A load balancer that selects its backends by tag reports zero droplets until
// something carries the tag, which is indistinguishable here from a balancer
// whose backends have all gone away. Both are worth looking at.
package loadbalancers

import (
	"context"
	"log/slog"
	"sync"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// activeStatus is the load balancer status that counts as up.
const activeStatus = "active"

// Metric descriptors. The label sets of statusDesc and dropletsDesc match the
// older exporter, so the descriptive labels live on an info metric of their
// own rather than widening them.
var (
	statusDesc = prometheus.NewDesc("digitalocean_loadbalancer_status",
		"The status of the load balancer, 1 if active.", []string{"id", "name", "ip"}, nil)
	dropletsDesc = prometheus.NewDesc("digitalocean_loadbalancer_droplets",
		"The number of droplets this load balancer is proxying to.",
		[]string{"id", "name", "ip"}, nil)
	sizeUnitsDesc = prometheus.NewDesc("digitalocean_loadbalancer_size_units",
		"Number of size units the load balancer is billed for.",
		[]string{"id", "name", "ip"}, nil)
	forwardingRulesDesc = prometheus.NewDesc("digitalocean_loadbalancer_forwarding_rules",
		"Number of forwarding rules configured on the load balancer.",
		[]string{"id", "name", "ip"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_loadbalancer_info",
		"Always 1. Its labels describe the load balancer's placement and configuration.",
		[]string{"id", "name", "ip", "region", "size", "type", "algorithm", "vpc_uuid"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	statusDesc, dropletsDesc, sizeUnitsDesc, forwardingRulesDesc, infoDesc,
}

// loadBalancer is what one refresh learned about a single load balancer.
type loadBalancer struct {
	id        string
	name      string
	ip        string
	region    string
	size      string
	lbType    string
	algorithm string
	vpcUUID   string

	status          float64
	droplets        float64
	sizeUnits       float64
	forwardingRules float64
}

// Collector reports the load balancers of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap []loadBalancer
}

// New returns a load balancer collector backed by client. The logger records
// what the scheduler never sees: a duplicate load balancer dropped from a list
// that shifted between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "loadbalancers" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous load balancers in place rather than reporting half an account.
func (c *Collector) Refresh(ctx context.Context) error {
	balancers, err := paging.All(ctx, c.logger, "load balancers",
		func(lb godo.LoadBalancer) string { return lb.ID }, c.client.LoadBalancers.List)
	if err != nil {
		return err
	}

	next := make([]loadBalancer, 0, len(balancers))
	for i := range balancers {
		next = append(next, newLoadBalancer(&balancers[i]))
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newLoadBalancer converts one API load balancer into its snapshot form.
func newLoadBalancer(lb *godo.LoadBalancer) loadBalancer {
	out := loadBalancer{
		id:              lb.ID,
		name:            lb.Name,
		ip:              lb.IP,
		size:            lb.SizeSlug,
		lbType:          lb.Type,
		algorithm:       lb.Algorithm,
		vpcUUID:         lb.VPCUUID,
		status:          boolToFloat(lb.Status == activeStatus),
		droplets:        float64(len(lb.DropletIDs)),
		sizeUnits:       float64(lb.SizeUnit),
		forwardingRules: float64(len(lb.ForwardingRules)),
	}
	if lb.Region != nil {
		out.region = lb.Region.Slug
	}
	return out
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no load balancers, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, lb := range snap {
		labels := []string{lb.id, lb.name, lb.ip}
		gauge(ch, statusDesc, lb.status, labels...)
		gauge(ch, dropletsDesc, lb.droplets, labels...)
		gauge(ch, sizeUnitsDesc, lb.sizeUnits, labels...)
		gauge(ch, forwardingRulesDesc, lb.forwardingRules, labels...)
		gauge(ch, infoDesc, 1, lb.id, lb.name, lb.ip, lb.region, lb.size, lb.lbType, lb.algorithm, lb.vpcUUID)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
