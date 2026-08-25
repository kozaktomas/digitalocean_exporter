// Package dropletmetrics collects what DigitalOcean's monitoring agent reports
// from inside each droplet: CPU time, memory, filesystems and load average.
//
// This is the only collector whose cost grows with the size of the account.
// The monitoring API answers one metric of one droplet per request, so a
// refresh spends one droplet listing plus one request per metric for every
// droplet, which is ten of them. Against a limit of 5000 requests an hour that is comfortable for a
// handful of droplets and impossible for a hundred, which is why the collector
// is off unless asked for and refreshes no faster than it has to.
//
// The API itself samples every two minutes, which also bounds how fresh a
// reading can be: the newest sample is up to one sampling interval old.
// Refreshing faster than that spends requests re-reading a sample that has not
// changed.
//
// A droplet reports nothing unless DigitalOcean's monitoring agent is
// installed and running on it. Such a droplet is not a failure: its fetch
// succeeds and simply returns no series, so it is up with no readings.
package dropletmetrics

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

// dropletsPerPage is how many droplets one page request asks for, which is the
// most the API allows.
const dropletsPerPage = 200

// window is how far back a metrics query reaches. The API samples every two
// minutes, so a window of several of them tolerates a late or skipped sample
// and still returns something current.
const window = 10 * time.Minute

// ErrNoDropletMeasured reports that every droplet's fetch failed, which points
// at the API rather than at any one droplet.
var ErrNoDropletMeasured = errors.New("no droplet could be measured")

// point is one sample ready to be emitted, kept in the form Collect needs so
// that Collect does no work beyond replaying it.
type point struct {
	desc      *prometheus.Desc
	valueType prometheus.ValueType
	labels    []string
	value     float64
}

// droplet is what one refresh learned about a single droplet.
type droplet struct {
	id   string
	name string
	// up is 0 when the last fetch for this droplet failed, in which case
	// points are whatever it last reported.
	up float64
	// sampled is the Unix time of the newest sample seen, or 0 when the
	// droplet returned no series at all.
	sampled float64
	points  []point
}

// reference is the identity of a droplet to measure.
type reference struct {
	id   string
	name string
}

// result is one droplet's fetch, successful or not.
type result struct {
	droplet droplet
	ref     reference
	err     error
}

// Collector reports the monitoring API's readings for every droplet.
type Collector struct {
	client      *godo.Client
	logger      *slog.Logger
	concurrency int

	mu   sync.RWMutex
	snap []droplet
}

// New returns a droplet metrics collector backed by client. Concurrency caps
// how many droplets are measured at once and is raised to 1 if it is lower;
// logger receives a warning for each droplet that could not be measured, since
// that failure never reaches the scheduler.
func New(client *godo.Client, concurrency int, logger *slog.Logger) *Collector {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Collector{client: client, logger: logger, concurrency: concurrency}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "dropletmetrics" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. A droplet that cannot be measured
// keeps whatever it last reported and is marked down; only a failure to list
// the droplets, or the failure of every one of them, fails the refresh as a
// whole. One droplet's failure must not cost the droplets that succeeded.
func (c *Collector) Refresh(ctx context.Context) error {
	refs, err := c.listDroplets(ctx)
	if err != nil {
		return err
	}

	results := c.measureAll(ctx, refs)
	c.merge(results)

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			c.logger.Warn("measuring a droplet failed",
				"droplet", r.ref.name, "id", r.ref.id, "error", r.err)
		}
	}
	if failed > 0 && failed == len(results) {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoDropletMeasured, failed, results[len(results)-1].err)
	}
	return nil
}

// listDroplets names every droplet in the account.
func (c *Collector) listDroplets(ctx context.Context) ([]reference, error) {
	opts := &godo.ListOptions{PerPage: dropletsPerPage}
	var refs []reference

	for {
		page, resp, err := c.client.Droplets.List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list droplets: %w", err)
		}
		for _, d := range page {
			refs = append(refs, reference{id: fmt.Sprint(d.ID), name: d.Name})
		}

		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return refs, nil
		}
		current, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, fmt.Errorf("next page of droplets: %w", err)
		}
		opts.Page = current + 1
	}
}

// measureAll measures every droplet, at most Concurrency of them at once. The
// requests of one droplet are sequential so that a large account cannot open
// len(specs) connections per droplet at the same time.
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
			results[i] = result{droplet: measured, ref: ref, err: err}
		}()
	}
	wg.Wait()
	return results
}

// measure fetches every metric of one droplet. Any failed request fails the
// droplet: a partial reading would mix fresh numbers with stale ones under the
// same timestamp.
func (c *Collector) measure(ctx context.Context, ref reference, start, end time.Time) (droplet, error) {
	out := droplet{id: ref.id, name: ref.name, up: 1}
	req := &godo.DropletMetricsRequest{HostID: ref.id, Start: start, End: end}

	for _, s := range specs {
		resp, _, err := s.fetch(ctx, c.client, req)
		if err != nil {
			return droplet{}, fmt.Errorf("fetch %s: %w", s.name, err)
		}
		samples, err := timeseries.Latest(resp)
		if err != nil {
			return droplet{}, fmt.Errorf("read %s: %w", s.name, err)
		}

		for _, sample := range samples {
			labels := make([]string, 0, len(identity)+len(s.seriesLabels))
			labels = append(labels, ref.id, ref.name)
			for _, name := range s.seriesLabels {
				labels = append(labels, sample.Label(name))
			}
			out.points = append(out.points,
				point{desc: s.desc, valueType: s.valueType, labels: labels, value: sample.Value})

			if at := float64(sample.Time.Unix()); at > out.sampled {
				out.sampled = at
			}
		}
	}
	return out, nil
}

// merge replaces the snapshot with the results, keeping the previous readings
// of any droplet whose fetch failed so that a graph shows a stale value rather
// than a gap. Droplets that have gone away are dropped.
func (c *Collector) merge(results []result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := make(map[string]droplet, len(c.snap))
	for _, d := range c.snap {
		previous[d.id] = d
	}

	next := make([]droplet, 0, len(results))
	for _, r := range results {
		if r.err == nil {
			next = append(next, r.droplet)
			continue
		}
		stale := previous[r.ref.id]
		stale.id, stale.name, stale.up = r.ref.id, r.ref.name, 0
		next = append(next, stale)
	}
	c.snap = next
}

// Collect implements collector.Collector. Before the first successful refresh,
// and on an account with no droplets, it emits nothing.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for _, d := range snap {
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, d.up, d.id, d.name)
		if d.sampled > 0 {
			ch <- prometheus.MustNewConstMetric(sampledDesc, prometheus.GaugeValue,
				d.sampled, d.id, d.name)
		}
		for _, p := range d.points {
			ch <- prometheus.MustNewConstMetric(p.desc, p.valueType, p.value, p.labels...)
		}
	}
}
