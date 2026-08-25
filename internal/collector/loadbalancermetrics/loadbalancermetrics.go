// Package loadbalancermetrics collects traffic through the account's load
// balancers from DigitalOcean's monitoring API: connections, HTTP response
// rates, frontend CPU and the health of each backend droplet.
//
// A load balancer cannot run node_exporter, so unlike droplet metrics none of
// this is available anywhere else. What it is worth most for is
// digitalocean_loadbalancer_droplets_health_checks, which names the individual
// backend that is failing rather than only reporting that the pool has shrunk.
//
// The cost is the same shape as the droplet metrics collector's — one request
// per metric per load balancer — but far smaller, since an account has orders
// of magnitude fewer load balancers than droplets. It is off by default all the
// same, so that enabling monitoring is always a deliberate act.
//
// An empty result is normal here rather than exceptional: a load balancer with
// no traffic has no HTTP response series at all, and a network load balancer
// has none of the HTTP metrics ever.
package loadbalancermetrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/timeseries"
)

// loadBalancersPerPage is how many load balancers one page request asks for,
// which is the most the API allows.
const loadBalancersPerPage = 200

// window is how far back a metrics query reaches. The API samples every two
// minutes, so a window of several of them tolerates a late or skipped sample
// and still returns something current.
const window = 10 * time.Minute

// ErrNoLoadBalancerMeasured reports that every load balancer's fetch failed,
// which points at the API rather than at any one load balancer.
var ErrNoLoadBalancerMeasured = errors.New("no load balancer could be measured")

// point is one sample ready to be emitted, kept in the form Collect needs so
// that Collect does no work beyond replaying it.
type point struct {
	desc   *prometheus.Desc
	labels []string
	value  float64
}

// loadBalancer is what one refresh learned about a single load balancer.
type loadBalancer struct {
	id   string
	name string
	// up is 0 when the last fetch for this load balancer failed, in which
	// case points are whatever it last reported.
	up float64
	// sampled is the Unix time of the newest sample seen, or 0 when the load
	// balancer returned no series at all.
	sampled float64
	points  []point
}

// reference is the identity of a load balancer to measure.
type reference struct {
	id   string
	name string
}

// result is one load balancer's fetch, successful or not.
type result struct {
	measured loadBalancer
	ref      reference
	err      error
}

// Collector reports the monitoring API's readings for every load balancer.
type Collector struct {
	client      *godo.Client
	logger      *slog.Logger
	concurrency int

	mu   sync.RWMutex
	snap []loadBalancer
}

// New returns a load balancer metrics collector backed by client. Concurrency
// caps how many load balancers are measured at once and is raised to 1 if it
// is lower; logger receives a warning for each one that could not be measured,
// since that failure never reaches the scheduler.
func New(client *godo.Client, concurrency int, logger *slog.Logger) *Collector {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Collector{client: client, logger: logger, concurrency: concurrency}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "loadbalancermetrics" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. A load balancer that cannot be
// measured keeps whatever it last reported and is marked down; only a failure
// to list them, or the failure of every one, fails the refresh as a whole.
func (c *Collector) Refresh(ctx context.Context) error {
	refs, err := c.listLoadBalancers(ctx)
	if err != nil {
		return err
	}

	results := c.measureAll(ctx, refs)
	c.merge(results)

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			c.logger.Warn("measuring a load balancer failed",
				"loadbalancer", r.ref.name, "id", r.ref.id, "error", r.err)
		}
	}
	if failed > 0 && failed == len(results) {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoLoadBalancerMeasured, failed, results[len(results)-1].err)
	}
	return nil
}

// listLoadBalancers names every load balancer in the account.
func (c *Collector) listLoadBalancers(ctx context.Context) ([]reference, error) {
	opts := &godo.ListOptions{PerPage: loadBalancersPerPage}
	var refs []reference

	for {
		page, resp, err := c.client.LoadBalancers.List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list load balancers: %w", err)
		}
		for _, lb := range page {
			refs = append(refs, reference{id: lb.ID, name: lb.Name})
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return refs, nil
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, fmt.Errorf("next page of load balancers: %w", err)
		}
		opts.Page = current + 1
	}
}

// measureAll measures every load balancer, at most Concurrency of them at
// once. The requests of one load balancer are sequential.
func (c *Collector) measureAll(ctx context.Context, refs []reference) []result {
	results := make([]result, len(refs))
	sem := make(chan struct{}, c.concurrency)
	end := time.Now()
	start := end.Add(-window)

	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			measured, err := c.measure(ctx, ref, start, end)
			results[i] = result{measured: measured, ref: ref, err: err}
		}()
	}
	wg.Wait()
	return results
}

// measure fetches every metric of one load balancer. Any failed request fails
// the load balancer: a partial reading would mix fresh numbers with stale ones
// under the same timestamp.
func (c *Collector) measure(
	ctx context.Context, ref reference, start, end time.Time,
) (loadBalancer, error) {
	out := loadBalancer{id: ref.id, name: ref.name, up: 1}
	req := &godo.LoadBalancerMetricsRequest{LoadBalancerID: ref.id, Start: start, End: end}

	for _, s := range specs {
		resp, _, err := s.fetch(ctx, c.client, req)
		if err != nil {
			return loadBalancer{}, fmt.Errorf("fetch %s: %w", s.name, err)
		}
		samples, err := timeseries.Latest(resp)
		if err != nil {
			return loadBalancer{}, fmt.Errorf("read %s: %w", s.name, err)
		}

		for _, sample := range samples {
			labels := make([]string, 0, len(identity)+len(s.seriesLabels))
			labels = append(labels, ref.id, ref.name)
			for _, name := range s.seriesLabels {
				labels = append(labels, sample.Label(name))
			}
			out.points = append(out.points,
				point{desc: s.desc, labels: labels, value: sample.Value})

			if at := float64(sample.Time.Unix()); at > out.sampled {
				out.sampled = at
			}
		}
	}
	return out, nil
}

// merge replaces the snapshot with the results, keeping the previous readings
// of any load balancer whose fetch failed so that a graph shows a stale value
// rather than a gap. Load balancers that have gone away are dropped.
func (c *Collector) merge(results []result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := make(map[string]loadBalancer, len(c.snap))
	for _, lb := range c.snap {
		previous[lb.id] = lb
	}

	next := make([]loadBalancer, 0, len(results))
	for _, r := range results {
		if r.err == nil {
			next = append(next, r.measured)
			continue
		}
		stale := previous[r.ref.id]
		stale.id, stale.name, stale.up = r.ref.id, r.ref.name, 0
		next = append(next, stale)
	}
	c.snap = next
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no load balancers, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, lb := range snap {
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, lb.up, lb.id, lb.name)
		if lb.sampled > 0 {
			ch <- prometheus.MustNewConstMetric(sampledDesc, prometheus.GaugeValue,
				lb.sampled, lb.id, lb.name)
		}
		for _, p := range lb.points {
			ch <- prometheus.MustNewConstMetric(p.desc, prometheus.GaugeValue, p.value, p.labels...)
		}
	}
}
