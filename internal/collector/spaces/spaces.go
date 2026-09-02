// Package spaces collects the size and object count of Spaces buckets.
//
// Both come from a single HEAD of each bucket: Spaces runs on the Ceph RADOS
// Gateway, which reports its own accounting in headers the S3 model does not
// define. See internal/spacesclient. Adding up a listing of every object would
// answer the same question, but it costs a request per thousand objects and
// stops working altogether on a bucket large enough, which is the whole reason
// this collector does not do it.
package spaces

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/spacesclient"
)

// Metric descriptors.
var (
	sizeDesc = prometheus.NewDesc("digitalocean_spaces_bucket_size_bytes",
		"Bytes stored in the bucket, as Spaces accounts for them.", []string{"bucket", "region"}, nil)
	objectsDesc = prometheus.NewDesc("digitalocean_spaces_bucket_objects",
		"Number of objects in the bucket.", []string{"bucket", "region"}, nil)
	upDesc = prometheus.NewDesc("digitalocean_spaces_bucket_up",
		"Whether the bucket's last measurement succeeded.", []string{"bucket", "region"}, nil)
)

// ErrNoBucketMeasured reports that not a single bucket could be measured.
var ErrNoBucketMeasured = errors.New("no bucket could be measured")

// Bucket names a bucket and the region it lives in.
type Bucket struct {
	// Name is the bucket name.
	Name string
	// Region is the Spaces region the bucket is served from.
	Region string
}

// ClientFactory hands out an S3 client for a region.
type ClientFactory interface {
	// Client returns a client addressing the given region.
	Client(region string) *s3.Client
}

// Config is everything the collector needs to run.
type Config struct {
	// Factory builds the per-region S3 clients.
	Factory ClientFactory
	// Buckets is the explicit list of buckets to measure. When empty the
	// collector discovers them, which needs a full-access key.
	Buckets []Bucket
	// Region is the region discovery talks to and the default for buckets
	// configured without one.
	Region string
	// Concurrency caps how many buckets are measured at once.
	Concurrency int
	// Logger receives a warning per bucket that could not be measured. Those
	// failures never reach the scheduler, so this is the only place they are
	// reported. Nil discards them.
	Logger *slog.Logger
}

// stats is what one refresh learned about one bucket.
type stats struct {
	size    float64
	objects float64
	up      bool
	// known reports whether the figures come from a successful measurement.
	// A bucket never measured reports its failure and nothing else, because a
	// zero size is indistinguishable from an empty bucket.
	known bool
}

// Collector reports the size and object count of Spaces buckets.
type Collector struct {
	factory     ClientFactory
	buckets     []Bucket
	region      string
	concurrency int
	logger      *slog.Logger

	mu sync.RWMutex
	// snap is keyed by the whole Bucket, not by its name: a name is only
	// unique within a region, and two buckets of the same name in different
	// regions are two buckets, reported under two label sets.
	snap map[Bucket]stats

	// locMu guards located.
	locMu sync.Mutex
	// located remembers where discovery found each bucket, keyed by name. A
	// bucket never changes region, so locating one is worth doing once rather
	// than once per bucket on every refresh. An entry survives only as long as
	// the bucket measures successfully: a region that turns out to be wrong,
	// or was assumed because locating the bucket failed, is looked up again on
	// the next refresh.
	located map[string]string
}

// New returns a Spaces collector built from cfg.
func New(cfg Config) *Collector {
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{
		factory:     cfg.Factory,
		buckets:     cfg.Buckets,
		region:      cfg.Region,
		concurrency: concurrency,
		logger:      logger,
	}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "spaces" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{sizeDesc, objectsDesc, upDesc} {
		ch <- d
	}
}

// Refresh implements collector.Collector. A bucket that cannot be measured
// keeps whatever it last reported and is marked down; only a failure to
// discover the buckets, or the failure of every one of them, fails the refresh
// as a whole.
func (c *Collector) Refresh(ctx context.Context) error {
	buckets, err := c.resolveBuckets(ctx)
	if err != nil {
		return err
	}

	results := c.measureAll(ctx, buckets)
	c.merge(results)
	if c.discovering() {
		c.rememberRegions(results)
	}

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			c.logger.Warn("measuring a Spaces bucket failed",
				"bucket", r.bucket.Name, "region", r.bucket.Region, "error", r.err)
		}
	}
	if failed > 0 && failed == len(results) {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoBucketMeasured, failed, results[len(results)-1].err)
	}
	return nil
}

// resolveBuckets returns the configured buckets, or discovers them.
func (c *Collector) resolveBuckets(ctx context.Context) ([]Bucket, error) {
	if !c.discovering() {
		out := make([]Bucket, 0, len(c.buckets))
		for _, b := range c.buckets {
			if b.Region == "" {
				b.Region = c.region
			}
			out = append(out, b)
		}
		return out, nil
	}
	return c.discover(ctx)
}

// discover lists every bucket of the account and locates each one. Listing all
// buckets is a full-access capability, so a key limited to specific buckets
// fails here and the error has to say what to do about it.
func (c *Collector) discover(ctx context.Context) ([]Bucket, error) {
	client := c.factory.Client(c.region)

	listed, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w "+
			"(a limited-access key cannot list buckets; name them with --collector.spaces.bucket)", err)
	}

	buckets := make([]Bucket, 0, len(listed.Buckets))
	for _, b := range listed.Buckets {
		name := aws.ToString(b.Name)
		buckets = append(buckets, Bucket{Name: name, Region: c.regionOf(ctx, client, name)})
	}
	return buckets, nil
}

// discovering reports whether the collector finds its buckets itself.
func (c *Collector) discovering() bool { return len(c.buckets) == 0 }

// regionOf resolves which region a discovered bucket is served from: from the
// cache when a previous refresh measured the bucket there, otherwise by asking.
//
// A bucket that cannot be located costs only itself. Failing the whole refresh
// meant one bucket in an unexpected state — moved, deleted between the listing
// and the location request, or dropped from a key's grants — blanked every
// other bucket in the account. The default region is assumed instead, and if
// that is wrong the measurement marks that one bucket down.
func (c *Collector) regionOf(ctx context.Context, client *s3.Client, name string) string {
	if region, ok := c.cachedRegion(name); ok {
		return region
	}
	region, err := c.locate(ctx, client, name)
	if err != nil {
		c.logger.Warn("locating a Spaces bucket failed, assuming the default region",
			"bucket", name, "region", c.region, "error", err)
		return c.region
	}
	return region
}

// locate resolves which region a discovered bucket is served from.
func (c *Collector) locate(ctx context.Context, client *s3.Client, name string) (string, error) {
	location, err := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("locate bucket %q: %w", name, err)
	}
	if region := string(location.LocationConstraint); region != "" {
		return region, nil
	}
	return c.region, nil
}

// cachedRegion returns where a bucket was last measured, if it was.
func (c *Collector) cachedRegion(name string) (string, bool) {
	c.locMu.Lock()
	defer c.locMu.Unlock()

	region, ok := c.located[name]
	return region, ok
}

// rememberRegions replaces the location cache with the regions this refresh
// proved right. A bucket that failed is left out, so it is located again next
// time rather than measured forever against a region it may not be in; a
// bucket that has disappeared from the listing leaves with it.
func (c *Collector) rememberRegions(results []result) {
	located := make(map[string]string, len(results))
	for _, r := range results {
		if r.err == nil {
			located[r.bucket.Name] = r.bucket.Region
		}
	}

	c.locMu.Lock()
	defer c.locMu.Unlock()
	c.located = located
}

// result is the outcome of measuring one bucket.
type result struct {
	bucket  Bucket
	size    float64
	objects float64
	err     error
}

// measureAll measures every bucket, at most Concurrency of them at a time.
// One bucket is one request, so buckets are the only axis there is.
func (c *Collector) measureAll(ctx context.Context, buckets []Bucket) []result {
	results := make([]result, len(buckets))
	sem := make(chan struct{}, c.concurrency)

	var wg sync.WaitGroup
	for i, b := range buckets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			usage, err := spacesclient.BucketUsage(ctx, c.factory.Client(b.Region), b.Name)
			results[i] = result{
				bucket: b, size: float64(usage.Bytes), objects: float64(usage.Objects), err: err,
			}
		}()
	}
	wg.Wait()
	return results
}

// merge builds the next snapshot. A bucket that failed carries its previous
// figures forward and is marked down, so one unreadable bucket never blanks
// the ones that were measured.
func (c *Collector) merge(results []result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := make(map[Bucket]stats, len(results))
	for _, r := range results {
		if r.err == nil {
			next[r.bucket] = stats{size: r.size, objects: r.objects, up: true, known: true}
			continue
		}
		previous := c.snap[r.bucket]
		previous.up = false
		next[r.bucket] = previous
	}
	c.snap = next
}

// Collect implements collector.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for b, s := range snap {
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, boolToFloat(s.up), b.Name, b.Region)
		if !s.known {
			continue
		}
		ch <- prometheus.MustNewConstMetric(sizeDesc, prometheus.GaugeValue, s.size, b.Name, b.Region)
		ch <- prometheus.MustNewConstMetric(objectsDesc, prometheus.GaugeValue, s.objects, b.Name, b.Region)
	}
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
