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

// Extended metric descriptors, read only with the extended flag set.
//
// The throughput and firewall metrics are rates: the API specification states
// the throughputs in bytes per second and the dropped packets per second, and
// the dropped bytes follow the packets. The TLS connection metrics are rates
// too — the specification calls the current one a "TLS connections rate", and
// the limit is the rate cap a size unit allows — but it names no unit for
// them, so their names carry none, matching frontend_connections_current. The
// durations are in seconds, stated by the specification.
var (
	requestsDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_http_requests_per_second",
		"Rate of HTTP requests received.", identity, nil)
	throughputHTTPDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_network_throughput_http_bytes_per_second",
		"HTTP throughput through the frontend.", identity, nil)
	throughputUDPDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_network_throughput_udp_bytes_per_second",
		"UDP throughput through the frontend.", identity, nil)
	throughputTCPDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_network_throughput_tcp_bytes_per_second",
		"TCP throughput through the frontend.", identity, nil)
	nlbThroughputTCPDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_nlb_tcp_network_throughput_bytes_per_second",
		"TCP throughput through a network load balancer's frontend.", identity, nil)
	nlbThroughputUDPDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_nlb_udp_network_throughput_bytes_per_second",
		"UDP throughput through a network load balancer's frontend.", identity, nil)
	firewallBytesDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_firewall_dropped_bytes_per_second",
		"Bytes dropped by the frontend firewall.", identity, nil)
	firewallPacketsDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_firewall_dropped_packets_per_second",
		"Packets dropped by the frontend firewall.", identity, nil)
	tlsConnectionsDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_tls_connections_current",
		"Rate of new TLS connections to the frontend.", identity, nil)
	tlsConnectionsLimitDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_tls_connections_limit",
		"Maximum TLS connection rate the frontend allows.", identity, nil)
	tlsExceedingDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_frontend_tls_connections_exceeding_rate_limit",
		"TLS connections the rate limit closed.", identity, nil)
	backendConnectionsDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_connections",
		"Active connections to a backend droplet.", labelsWith("server"), nil)
	queueSizeDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_queue_size",
		"HTTP requests queued waiting for a backend.", identity, nil)
	backendResponsesDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_responses_per_second",
		"Rate of backend HTTP responses, by code class.",
		labelsWith("code"), nil)
	sessionAvgDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_session_duration_avg_seconds",
		"Average backend session duration.", identity, nil)
	sessionP50Desc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_session_duration_p50_seconds",
		"Median backend session duration.", identity, nil)
	sessionP95Desc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_session_duration_p95_seconds",
		"95th percentile backend session duration.", identity, nil)
	responseTimeAvgDesc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_response_time_avg_seconds",
		"Average backend response time.", identity, nil)
	responseTimeP50Desc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_response_time_p50_seconds",
		"Median backend response time.", identity, nil)
	responseTimeP99Desc = prometheus.NewDesc(
		"digitalocean_loadbalancer_droplets_http_response_time_p99_seconds",
		"99th percentile backend response time.", identity, nil)
)

// descriptors lists every metric the collector can emit. The extended ones are
// described whether or not they are read: a descriptor declares what a metric
// would look like, and the dashboards are held against this list, so a panel
// over an extended metric is checked even in a build that never enables it.
var descriptors = []*prometheus.Desc{
	connectionsDesc, connectionsLimitDesc, cpuDesc, responsesDesc,
	healthDesc, downtimeDesc, responseTimeDesc, upDesc, sampledDesc,
	requestsDesc, throughputHTTPDesc, throughputUDPDesc, throughputTCPDesc,
	nlbThroughputTCPDesc, nlbThroughputUDPDesc, firewallBytesDesc, firewallPacketsDesc,
	tlsConnectionsDesc, tlsConnectionsLimitDesc, tlsExceedingDesc,
	backendConnectionsDesc, queueSizeDesc, backendResponsesDesc,
	sessionAvgDesc, sessionP50Desc, sessionP95Desc,
	responseTimeAvgDesc, responseTimeP50Desc, responseTimeP99Desc,
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

// specs is every metric fetched per load balancer whatever the flags say, and
// so also the base request cost of one refresh: len(specs) requests for each
// load balancer, plus len(extendedSpecs) more with the extended flag set.
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

// extendedSpecs is the rest of what the monitoring API offers per load
// balancer, fetched only with the extended flag set: each is one more request
// per load balancer per refresh, which is why they are opt-in.
//
// Several of them apply to one kind of load balancer only — the nlb throughputs
// and the firewall drops to network load balancers, everything HTTP or TLS to
// regional ones. The API answers an inapplicable metric with an empty result,
// not an error, so a mixed account is not a stream of failures.
var extendedSpecs = []spec{
	{
		name: "frontend_http_requests_per_second", desc: requestsDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendHttpRequestsPerSecond(ctx, r)
		},
	},
	{
		name: "frontend_network_throughput_http", desc: throughputHTTPDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendNetworkThroughputHttp(ctx, r)
		},
	},
	{
		name: "frontend_network_throughput_udp", desc: throughputUDPDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendNetworkThroughputUdp(ctx, r)
		},
	},
	{
		name: "frontend_network_throughput_tcp", desc: throughputTCPDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendNetworkThroughputTcp(ctx, r)
		},
	},
	{
		name: "frontend_nlb_tcp_network_throughput", desc: nlbThroughputTCPDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendNlbTcpNetworkThroughput(ctx, r)
		},
	},
	{
		name: "frontend_nlb_udp_network_throughput", desc: nlbThroughputUDPDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendNlbUdpNetworkThroughput(ctx, r)
		},
	},
	{
		name: "frontend_firewall_dropped_bytes", desc: firewallBytesDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendFirewallDroppedBytes(ctx, r)
		},
	},
	{
		name: "frontend_firewall_dropped_packets", desc: firewallPacketsDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendFirewallDroppedPackets(ctx, r)
		},
	},
	{
		name: "frontend_tls_connections_current", desc: tlsConnectionsDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendTlsConnectionsCurrent(ctx, r)
		},
	},
	{
		name: "frontend_tls_connections_limit", desc: tlsConnectionsLimitDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendTlsConnectionsLimit(ctx, r)
		},
	},
	{
		name: "frontend_tls_connections_exceeding_rate_limit", desc: tlsExceedingDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerFrontendTlsConnectionsExceedingRateLimit(ctx, r)
		},
	},
	{
		name: "droplets_connections", desc: backendConnectionsDesc,
		seriesLabels: []string{"server"},
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsConnections(ctx, r)
		},
	},
	{
		name: "droplets_queue_size", desc: queueSizeDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsQueueSize(ctx, r)
		},
	},
	{
		name: "droplets_http_responses", desc: backendResponsesDesc,
		seriesLabels: []string{"code"},
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpResponses(ctx, r)
		},
	},
	{
		name: "droplets_http_session_duration_avg", desc: sessionAvgDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpSessionDurationAvg(ctx, r)
		},
	},
	{
		name: "droplets_http_session_duration_50p", desc: sessionP50Desc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpSessionDuration50P(ctx, r)
		},
	},
	{
		name: "droplets_http_session_duration_95p", desc: sessionP95Desc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpSessionDuration95P(ctx, r)
		},
	},
	{
		name: "droplets_http_response_time_avg", desc: responseTimeAvgDesc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpResponseTimeAvg(ctx, r)
		},
	},
	{
		name: "droplets_http_response_time_50p", desc: responseTimeP50Desc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpResponseTime50P(ctx, r)
		},
	},
	{
		name: "droplets_http_response_time_99p", desc: responseTimeP99Desc,
		fetch: func(ctx context.Context, c *godo.Client, r *godo.LoadBalancerMetricsRequest) (
			*godo.MetricsResponse, *godo.Response, error,
		) {
			return c.Monitoring.GetLoadBalancerDropletsHttpResponseTime99P(ctx, r)
		},
	},
}
