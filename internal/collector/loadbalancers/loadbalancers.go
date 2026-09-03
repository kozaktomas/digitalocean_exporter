// Package loadbalancers collects the load balancers of the account: whether
// each one is active, how many droplets it proxies to, what it costs, and how
// it is configured — forwarding rules with their certificate, the health
// check, and its own firewall's rule counts. All of it comes from the one
// list response; none of the configuration series costs an extra request.
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
	"strconv"
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
		"Always 1. Its labels describe the load balancer's placement and configuration; "+
			"tag is the droplet tag it selects its backends by, empty when they are listed by ID.",
		[]string{"id", "name", "ip", "region", "size", "type", "algorithm", "vpc_uuid", "tag"}, nil)
	forwardingRuleInfoDesc = prometheus.NewDesc("digitalocean_loadbalancer_forwarding_rule_info",
		"Always 1, one series per forwarding rule. certificate_id joins the rule to "+
			"digitalocean_certificate_expiry_timestamp_seconds on that collector's id label.",
		[]string{
			"id", "name", "ip",
			"entry_protocol", "entry_port", "target_protocol", "target_port",
			"certificate_id", "tls_passthrough",
		}, nil)
	healthCheckInfoDesc = prometheus.NewDesc("digitalocean_loadbalancer_health_check_info",
		"Always 1. Its labels describe how the load balancer probes its backends.",
		[]string{"id", "name", "ip", "protocol", "port", "path"}, nil)
	healthCheckIntervalDesc = prometheus.NewDesc("digitalocean_loadbalancer_health_check_interval_seconds",
		"Seconds between two health checks of the same backend.",
		[]string{"id", "name", "ip"}, nil)
	healthCheckTimeoutDesc = prometheus.NewDesc("digitalocean_loadbalancer_health_check_timeout_seconds",
		"Seconds the health check waits for a backend to respond before counting a failure.",
		[]string{"id", "name", "ip"}, nil)
	healthCheckHealthyDesc = prometheus.NewDesc("digitalocean_loadbalancer_health_check_healthy_threshold",
		"Consecutive successful health checks before a backend is put back into rotation.",
		[]string{"id", "name", "ip"}, nil)
	healthCheckUnhealthyDesc = prometheus.NewDesc("digitalocean_loadbalancer_health_check_unhealthy_threshold",
		"Consecutive failed health checks before a backend is taken out of rotation.",
		[]string{"id", "name", "ip"}, nil)
	firewallRulesDesc = prometheus.NewDesc("digitalocean_loadbalancer_firewall_rules",
		"Number of rules of that kind on the load balancer's own firewall: "+
			"kind is allow or deny, and both are 0 when no firewall is configured.",
		[]string{"id", "name", "ip", "kind"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	statusDesc, dropletsDesc, sizeUnitsDesc, forwardingRulesDesc, infoDesc,
	forwardingRuleInfoDesc, healthCheckInfoDesc, healthCheckIntervalDesc,
	healthCheckTimeoutDesc, healthCheckHealthyDesc, healthCheckUnhealthyDesc,
	firewallRulesDesc,
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
	tag       string

	status          float64
	droplets        float64
	sizeUnits       float64
	forwardingRules float64

	rules         []forwardingRule
	health        *healthCheck
	firewallAllow float64
	firewallDeny  float64
}

// forwardingRule is one forwarding rule's configuration, already in label
// form. The API keeps the entry protocol and port unique within one load
// balancer, which is what keeps these series distinct.
type forwardingRule struct {
	entryProtocol  string
	entryPort      string
	targetProtocol string
	targetPort     string
	certificateID  string
	tlsPassthrough string
}

// healthCheck is the load balancer's health check configuration. It is a
// pointer on the snapshot because a load balancer can carry none — a
// REGIONAL_NETWORK one passes packets through — and absent configuration is
// no series rather than zeros.
type healthCheck struct {
	protocol string
	port     string
	path     string

	interval  float64
	timeout   float64
	healthy   float64
	unhealthy float64
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
		tag:             lb.Tag,
		status:          boolToFloat(lb.Status == activeStatus),
		droplets:        float64(len(lb.DropletIDs)),
		sizeUnits:       float64(lb.SizeUnit),
		forwardingRules: float64(len(lb.ForwardingRules)),
		rules:           newForwardingRules(lb.ForwardingRules),
		health:          newHealthCheck(lb.HealthCheck),
	}
	if lb.Region != nil {
		out.region = lb.Region.Slug
	}
	if lb.Firewall != nil {
		out.firewallAllow = float64(len(lb.Firewall.Allow))
		out.firewallDeny = float64(len(lb.Firewall.Deny))
	}
	return out
}

// newForwardingRules converts the API forwarding rules into their label form.
func newForwardingRules(rules []godo.ForwardingRule) []forwardingRule {
	out := make([]forwardingRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, forwardingRule{
			entryProtocol:  rule.EntryProtocol,
			entryPort:      strconv.Itoa(rule.EntryPort),
			targetProtocol: rule.TargetProtocol,
			targetPort:     strconv.Itoa(rule.TargetPort),
			certificateID:  rule.CertificateID,
			tlsPassthrough: strconv.FormatBool(rule.TlsPassthrough),
		})
	}
	return out
}

// newHealthCheck converts the API health check into its snapshot form. A load
// balancer without one yields nil, which Collect reads as nothing to emit.
func newHealthCheck(check *godo.HealthCheck) *healthCheck {
	if check == nil {
		return nil
	}
	return &healthCheck{
		protocol:  check.Protocol,
		port:      strconv.Itoa(check.Port),
		path:      check.Path,
		interval:  float64(check.CheckIntervalSeconds),
		timeout:   float64(check.ResponseTimeoutSeconds),
		healthy:   float64(check.HealthyThreshold),
		unhealthy: float64(check.UnhealthyThreshold),
	}
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

	for i := range snap {
		lb := &snap[i]
		gauge(ch, statusDesc, lb.status, lb.id, lb.name, lb.ip)
		gauge(ch, dropletsDesc, lb.droplets, lb.id, lb.name, lb.ip)
		gauge(ch, sizeUnitsDesc, lb.sizeUnits, lb.id, lb.name, lb.ip)
		gauge(ch, forwardingRulesDesc, lb.forwardingRules, lb.id, lb.name, lb.ip)
		gauge(ch, infoDesc, 1,
			lb.id, lb.name, lb.ip, lb.region, lb.size, lb.lbType, lb.algorithm, lb.vpcUUID, lb.tag)
		gauge(ch, firewallRulesDesc, lb.firewallAllow, lb.id, lb.name, lb.ip, "allow")
		gauge(ch, firewallRulesDesc, lb.firewallDeny, lb.id, lb.name, lb.ip, "deny")
		for _, rule := range lb.rules {
			gauge(ch, forwardingRuleInfoDesc, 1, lb.id, lb.name, lb.ip,
				rule.entryProtocol, rule.entryPort, rule.targetProtocol, rule.targetPort,
				rule.certificateID, rule.tlsPassthrough)
		}
		collectHealthCheck(ch, lb)
	}
}

// collectHealthCheck emits the health check series of one load balancer, or
// nothing when it has no health check configured.
func collectHealthCheck(ch chan<- prometheus.Metric, lb *loadBalancer) {
	if lb.health == nil {
		return
	}
	gauge(ch, healthCheckInfoDesc, 1, lb.id, lb.name, lb.ip,
		lb.health.protocol, lb.health.port, lb.health.path)
	gauge(ch, healthCheckIntervalDesc, lb.health.interval, lb.id, lb.name, lb.ip)
	gauge(ch, healthCheckTimeoutDesc, lb.health.timeout, lb.id, lb.name, lb.ip)
	gauge(ch, healthCheckHealthyDesc, lb.health.healthy, lb.id, lb.name, lb.ip)
	gauge(ch, healthCheckUnhealthyDesc, lb.health.unhealthy, lb.id, lb.name, lb.ip)
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
