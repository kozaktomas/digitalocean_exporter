package loadbalancermetrics

import (
	"context"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"
)

// identity is the label set every metric here starts with. The series labels
// the monitoring API adds, if any, follow it.
var identity = []string{"id", "name"}

// labelsWith returns the identity labels followed by extra, always in a slice
// of its own. Appending straight onto identity would be a trap: the moment it
// has spare capacity, two descriptors built that way share a backing array and
// the second silently overwrites the first one's labels.
func labelsWith(extra ...string) []string {
	labels := make([]string, 0, len(identity)+len(extra))
	labels = append(labels, identity...)
	return append(labels, extra...)
}

// Metric descriptors.
//
// The units come from DigitalOcean's own API specification, which states them
// for some of these metrics and not for others. Where it says only "status" —
// the health check and the downtime of a backend — no unit is claimed here
// either, and the help text says what the observed values mean instead.
//
// frontend_http_responses is a rate, not a running total: the specification
// calls it the "rate of response code", so it is a gauge of responses per
// second rather than a counter.
var (
	connectionsDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_connections_current",
		"Active connections to the load balancer's frontend.", identity, nil)
	connectionsLimitDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_connections_limit",
		"Maximum active connections the frontend allows.", identity, nil)
	cpuDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_cpu_utilization_percent",
		"Average CPU utilization of the frontend, in percent.", identity, nil)
	responsesDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_http_responses_per_second",
		"Rate of HTTP responses served, by code class.",
		labelsWith("code"), nil)
	healthDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_health_checks",
		"Health check status of a backend droplet; 100 when healthy.",
		labelsWith("server"), nil)
	downtimeDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_downtime",
		"Downtime status of a backend droplet; 0 when it is up.",
		labelsWith("server"), nil)
	responseTimeDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_response_time_p95_seconds",
		"95th percentile backend response time.", identity, nil)
	upDesc = prometheus.NewDesc("digitalocean_loadbalancer_metrics_up",
		"Whether the load balancer's last metrics fetch succeeded.", identity, nil)
	sampledDesc = prometheus.NewDesc("digitalocean_loadbalancer_metrics_timestamp_seconds",
		"Unix time of the newest sample returned.", identity, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{
	connectionsDesc, connectionsLimitDesc, cpuDesc, responsesDesc,
	healthDesc, downtimeDesc, responseTimeDesc, upDesc, sampledDesc,
}

// fetcher asks the monitoring API for one metric of one load balancer.
type fetcher func(context.Context, *godo.Client, *godo.LoadBalancerMetricsRequest) (
	*godo.MetricsResponse, *godo.Response, error)

// spec describes one metric: where it comes from, which descriptor it feeds
// and which of the series' own labels are appended to the load balancer's
// identity.
type spec struct {
	// name identifies the metric in error messages.
	name string
	// desc is the descriptor the samples are emitted against.
	desc *prometheus.Desc
	// seriesLabels are read off each returned series, in order, and appended
	// to the load balancer's id and name. The API also labels most of these
	// series with lb_id and region, which are left out: the id is already
	// there and the region belongs to the inventory collector's info metric.
	seriesLabels []string
	// fetch performs the request.
	fetch fetcher
}

// specs is every metric fetched per load balancer, and so also the request
// cost of one refresh: len(specs) requests for each load balancer.
//
// Every one of these is a gauge. Nothing the load balancer monitoring API
// returns is cumulative, so none of them is a counter.
var specs = []spec{
	{
		name: "frontend_connections_current", desc: connectionsDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendConnectionsCurrent(ctx, r)
		},
	},
	{
		name: "frontend_connections_limit", desc: connectionsLimitDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendConnectionsLimit(ctx, r)
		},
	},
	{
		name: "frontend_cpu_utilization", desc: cpuDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendCpuUtilization(ctx, r)
		},
	},
	{
		name: "frontend_http_responses", desc: responsesDesc,
		seriesLabels: []string{"code"},
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendHttpResponses(ctx, r)
		},
	},
	{
		name: "droplets_health_checks", desc: healthDesc,
		seriesLabels: []string{"server"},
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHealthChecks(ctx, r)
		},
	},
	{
		name: "droplets_downtime", desc: downtimeDesc,
		seriesLabels: []string{"server"},
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsDowntime(ctx, r)
		},
	},
	{
		name: "droplets_http_response_time_95p", desc: responseTimeDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpResponseTime95P(ctx, r)
		},
	},
}
