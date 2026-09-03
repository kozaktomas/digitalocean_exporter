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
//
// A set of load balancers whose refresh does not fit in the collector's
// timeout is a failure and says so, rather than reporting the ones it reached
// and leaving the rest silently unmeasured. The starting point of the fan-out
// moves on from one refresh to the next, so a set slightly too large for its
// timeout covers every load balancer over a few refreshes instead of measuring
// the head of the list forever.
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

	"github.com/kozaktomas/digitalocean_exporter/internal/filter"
	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
	"github.com/kozaktomas/digitalocean_exporter/internal/timeseries"
)

// window is how far back a metrics query reaches. The API samples every two
// minutes, so a window of several of them tolerates a late or skipped sample
// and still returns something current.
const window = 10 * time.Minute

// ErrNoLoadBalancerMeasured reports that every load balancer's fetch failed,
// which points at the API rather than at any one load balancer.
var ErrNoLoadBalancerMeasured = errors.New("no load balancer could be measured")

// ErrRefreshCutShort reports that the refresh ran out of time before it had
// been through every load balancer. The ones it did reach keep their fresh
// readings, but the refresh is a failure: part of the account is unmeasured,
// and a snapshot that covers some of it must not be reported as a complete
// one.
var ErrRefreshCutShort = errors.New("refresh cut short")

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
	filter      filter.Filter

	mu   sync.RWMutex
	snap []loadBalancer
	// cursor is where the next fan-out starts, as an index into the load
	// balancer listing. It only moves when a refresh could not get through
	// the whole list, and then by exactly what it did get through.
	cursor int
}

// New returns a load balancer metrics collector backed by client, measuring
// only the load balancers f matches — a rejected one is not measured at all,
// so the filter cuts this collector's request cost, not just its output.
// Concurrency caps how many load balancers are measured at once and is raised
// to 1 if it is lower; logger receives a warning for each one that could not
// be measured, since that failure never reaches the scheduler.
func New(client *godo.Client, concurrency int, f filter.Filter, logger *slog.Logger) *Collector {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Collector{client: client, logger: logger, concurrency: concurrency, filter: f}
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
// measured keeps whatever it last reported and is marked down; a failure to
// list them, the failure of every one, or a refresh that ran out of time
// before reaching them all fails the refresh as a whole.
func (c *Collector) Refresh(ctx context.Context) error {
	refs, err := c.listLoadBalancers(ctx)
	if err != nil {
		return err
	}

	results := c.measureAll(ctx, c.rotate(refs))
	c.merge(results)
	return c.report(ctx, results)
}

// report logs every load balancer that could not be measured, moves the
// rotation past the ones this refresh got through, and returns the failures
// the scheduler has to hear about.
//
// A refresh whose context is done is one of them however many answered: the
// rest are unmeasured, which is what
// digitalocean_exporter_collector_success 0 is for. Only a refresh that got
// through the whole list is a success.
func (c *Collector) report(ctx context.Context, results []result) error {
	measured, reached := 0, 0
	for _, r := range results {
		if r.err == nil {
			measured++
			reached++
			continue
		}
		if !starved(r.err) {
			reached++
		}
		c.logger.Warn("measuring a load balancer failed",
			"loadbalancer", r.ref.name, "id", r.ref.id, "error", r.err)
	}
	c.advance(reached, len(results))

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: measured %d of %d load balancers: %w",
			ErrRefreshCutShort, measured, len(results), err)
	}
	if measured == 0 && len(results) > 0 {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoLoadBalancerMeasured, len(results), results[len(results)-1].err)
	}
	return nil
}

// starved reports whether err is nothing but the refresh running out of time
// before this load balancer had its turn. Such a one was never measured, so
// the rotation must not move past it; one that failed on its own — a 500, a
// response that would not parse — was, and gains nothing from being tried
// first next time.
func starved(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// rotate returns refs starting at the cursor, so the load balancers a
// cut-short refresh never reached are the ones the next refresh measures
// first. A refresh that gets through the whole list leaves the cursor where it
// was and so keeps the listing's own order.
func (c *Collector) rotate(refs []reference) []reference {
	if len(refs) == 0 {
		return refs
	}

	c.mu.RLock()
	from := c.cursor % len(refs)
	c.mu.RUnlock()

	rotated := make([]reference, 0, len(refs))
	rotated = append(rotated, refs[from:]...)
	return append(rotated, refs[:from]...)
}

// advance moves the cursor past the load balancers this refresh reached, so
// the next one continues where this one stopped. The listing can have changed
// size in between, which the modulo in rotate absorbs.
func (c *Collector) advance(by, total int) {
	if total == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor = (c.cursor + by) % total
}

// listLoadBalancers names every load balancer the filter admits. Filtering
// here rather than after measuring is what keeps a filtered account cheap:
// only the load balancers that pass are measured at all.
func (c *Collector) listLoadBalancers(ctx context.Context) ([]reference, error) {
	balancers, err := paging.All(ctx, c.logger, "load balancers",
		func(lb godo.LoadBalancer) string { return lb.ID }, c.client.LoadBalancers.List)
	if err != nil {
		return nil, err
	}

	refs := make([]reference, 0, len(balancers))
	for i := range balancers {
		if !c.filter.Match(balancers[i].Tags, regionSlug(&balancers[i])) {
			continue
		}
		refs = append(refs, reference{id: balancers[i].ID, name: balancers[i].Name})
	}
	return refs, nil
}

// regionSlug names the region a load balancer lies in, or "" when the API
// reported none.
func regionSlug(lb *godo.LoadBalancer) string {
	if lb.Region == nil {
		return ""
	}
	return lb.Region.Slug
}

// measureAll measures the load balancers in the order given, at most
// Concurrency of them at once. The requests of one load balancer are
// sequential.
//
// The work goes to a fixed set of workers through a queue rather than to one
// goroutine per load balancer, so the order refs are in is the order they are
// measured in. That is what makes rotating the starting point mean anything:
// with a goroutine each, which of them a refresh gets through before its
// deadline would be the Go scheduler's decision.
//
// Once the context is done nothing more is handed out: one that never got its
// turn reports that rather than spending a request on a call that could only
// fail.
func (c *Collector) measureAll(ctx context.Context, refs []reference) []result {
	results := make([]result, len(refs))
	end := time.Now()
	start := end.Add(-window)

	queue := make(chan int)
	var wg sync.WaitGroup
	for range min(c.concurrency, len(refs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				measured, err := c.measure(ctx, refs[i], start, end)
				results[i] = result{measured: measured, ref: refs[i], err: err}
			}
		}()
	}

	queued := len(refs)
dispatch:
	for i := range refs {
		select {
		case queue <- i:
		case <-ctx.Done():
			queued = i
			break dispatch
		}
	}
	close(queue)
	wg.Wait()

	for i := queued; i < len(refs); i++ {
		results[i] = result{ref: refs[i], err: fmt.Errorf("not measured: %w", ctx.Err())}
	}
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
