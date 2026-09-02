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
// succeeds and simply returns no series, so it is up with no readings. An
// account where that is true of most droplets can set AgentOnly and pay for
// the ones that answer only.
//
// A fleet whose refresh does not fit in the collector's timeout is a failure
// and says so, rather than reporting the droplets it reached and leaving the
// rest silently unmeasured. The starting point of the fan-out moves on from
// one refresh to the next, so a fleet slightly too large for its timeout
// covers every droplet over a few refreshes instead of measuring the head of
// the list forever.
package dropletmetrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// ErrRefreshCutShort reports that the refresh ran out of time before it had
// been through every droplet. The droplets it did reach keep their fresh
// readings, but the refresh is a failure: some of the fleet is unmeasured, and
// a snapshot that covers part of the account must not be reported as a
// complete one.
var ErrRefreshCutShort = errors.New("refresh cut short")

// monitoringFeature is the feature a droplet's listing carries when the
// droplet was created with DigitalOcean's monitoring agent.
const monitoringFeature = "monitoring"

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

// Config is what the droplet metrics collector needs beyond an API client.
type Config struct {
	// Concurrency caps how many droplets are measured at once and is raised
	// to 1 if it is lower.
	Concurrency int
	// AgentOnly skips droplets whose listing does not report the monitoring
	// feature, saving len(specs) requests each. It is opt-in because the
	// feature says only that the droplet was created with the agent: one
	// installed afterwards does not set it, and such a droplet would then go
	// unmeasured although it has readings.
	AgentOnly bool
	// Logger receives a warning for each droplet that could not be measured,
	// since that failure never reaches the scheduler.
	Logger *slog.Logger
}

// Collector reports the monitoring API's readings for every droplet.
type Collector struct {
	client      *godo.Client
	logger      *slog.Logger
	concurrency int
	agentOnly   bool

	mu   sync.RWMutex
	snap []droplet
	// cursor is where the next fan-out starts, as an index into the droplet
	// listing. It only moves when a refresh could not get through the whole
	// list, and then by exactly what it did get through.
	cursor int
}

// New returns a droplet metrics collector backed by client.
func New(client *godo.Client, cfg Config) *Collector {
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	return &Collector{
		client:      client,
		logger:      cfg.Logger,
		concurrency: concurrency,
		agentOnly:   cfg.AgentOnly,
	}
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
// keeps whatever it last reported and is marked down; a failure to list the
// droplets, the failure of every one of them, or a refresh that ran out of
// time before reaching them all fails the refresh as a whole. One droplet's
// failure must not cost the droplets that succeeded.
func (c *Collector) Refresh(ctx context.Context) error {
	refs, err := c.listDroplets(ctx)
	if err != nil {
		return err
	}

	results := c.measureAll(ctx, c.rotate(refs))
	c.merge(results)
	return c.report(ctx, results)
}

// report logs every droplet that could not be measured, moves the rotation
// past the ones this refresh got through, and returns the failures the
// scheduler has to hear about.
//
// A refresh whose context is done is one of them however many droplets
// answered: the rest of the fleet is unmeasured, which is what
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
		c.logger.Warn("measuring a droplet failed",
			"droplet", r.ref.name, "id", r.ref.id, "error", r.err)
	}
	c.advance(reached, len(results))

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: measured %d of %d droplets: %w",
			ErrRefreshCutShort, measured, len(results), err)
	}
	if measured == 0 && len(results) > 0 {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoDropletMeasured, len(results), results[len(results)-1].err)
	}
	return nil
}

// starved reports whether err is nothing but the refresh running out of time
// before this droplet had its turn. Such a droplet was never measured, so the
// rotation must not move past it; a droplet that failed on its own — a 500, a
// response that would not parse — was, and gains nothing from being tried
// first next time.
func starved(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// rotate returns refs starting at the cursor, so the droplets a cut-short
// refresh never reached are the ones the next refresh measures first. A
// refresh that gets through the whole list leaves the cursor where it was and
// so keeps the listing's own order.
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

// advance moves the cursor past the droplets this refresh reached, so the next
// one continues where this one stopped. The listing can have changed size in
// between, which the modulo in rotate absorbs.
func (c *Collector) advance(by, total int) {
	if total == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor = (c.cursor + by) % total
}

// listDroplets names every droplet in the account, or, with AgentOnly set,
// only those whose listing reports the monitoring agent.
func (c *Collector) listDroplets(ctx context.Context) ([]reference, error) {
	opts := &godo.ListOptions{PerPage: dropletsPerPage}
	var refs []reference

	for {
		page, resp, err := c.client.Droplets.List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list droplets: %w", err)
		}
		for _, d := range page {
			if c.agentOnly && !slices.Contains(d.Features, monitoringFeature) {
				continue
			}
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

// measureAll measures the droplets in the order given, at most Concurrency of
// them at once. The requests of one droplet are sequential so that a large
// account cannot open len(specs) connections per droplet at the same time.
//
// The work goes to a fixed set of workers through a queue rather than to one
// goroutine per droplet, so the order refs are in is the order they are
// measured in. That is what makes rotating the starting point mean anything:
// with a goroutine each, which droplets a refresh gets through before its
// deadline would be the Go scheduler's decision.
//
// Once the context is done nothing more is handed out: a droplet that never
// got its turn reports that rather than spending a request on a call that
// could only fail.
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
				results[i] = result{droplet: measured, ref: refs[i], err: err}
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
