// Package spaces collects the size and object count of Spaces buckets.
//
// DigitalOcean publishes no bucket size anywhere in its API, so the only way
// to learn one is to list every object and add the sizes up. That takes
// minutes on a large bucket, which is why this collector refreshes on a long
// interval of its own and why a scrape only ever reads the resulting snapshot.
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
)

// Metric descriptors.
var (
	sizeDesc = prometheus.NewDesc("digitalocean_spaces_bucket_size_bytes",
		"Total size of every object in the bucket.", []string{"bucket", "region"}, nil)
	objectsDesc = prometheus.NewDesc("digitalocean_spaces_bucket_objects",
		"Number of objects in the bucket.", []string{"bucket", "region"}, nil)
	upDesc = prometheus.NewDesc("digitalocean_spaces_bucket_up",
		"Whether the bucket's last listing succeeded.", []string{"bucket", "region"}, nil)
)

// ErrNoBucketListed reports that not a single bucket could be listed.
var ErrNoBucketListed = errors.New("no bucket could be listed")

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
	// Concurrency caps how many buckets are listed at once.
	Concurrency int
	// Logger receives a warning per bucket that could not be listed. Those
	// failures never reach the scheduler, so this is the only place they are
	// reported. Nil discards them.
	Logger *slog.Logger
}

// stats is what one refresh learned about one bucket.
type stats struct {
	region  string
	size    float64
	objects float64
	up      bool
	// known reports whether the figures come from a successful listing. A
	// bucket that has never listed reports its failure and nothing else,
	// because a zero size is indistinguishable from an empty bucket.
	known bool
}

// Collector reports the size and object count of Spaces buckets.
type Collector struct {
	factory     ClientFactory
	buckets     []Bucket
	region      string
	concurrency int
	logger      *slog.Logger

	mu   sync.RWMutex
	snap map[string]stats
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

// Refresh implements collector.Collector. A bucket that cannot be listed keeps
// whatever it last reported and is marked down; only a failure to discover the
// buckets, or the failure of every one of them, fails the refresh as a whole.
func (c *Collector) Refresh(ctx context.Context) error {
	buckets, err := c.resolveBuckets(ctx)
	if err != nil {
		return err
	}

	results := c.listAll(ctx, buckets)
	c.merge(results)

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			c.logger.Warn("listing a Spaces bucket failed",
				"bucket", r.bucket.Name, "region", r.bucket.Region, "error", r.err)
		}
	}
	if failed > 0 && failed == len(results) {
		return fmt.Errorf("%w: %d attempted, last error: %w",
			ErrNoBucketListed, failed, results[len(results)-1].err)
	}
	return nil
}

// resolveBuckets returns the configured buckets, or discovers them.
func (c *Collector) resolveBuckets(ctx context.Context) ([]Bucket, error) {
	if len(c.buckets) > 0 {
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
		region, locErr := c.locate(ctx, client, name)
		if locErr != nil {
			return nil, locErr
		}
		buckets = append(buckets, Bucket{Name: name, Region: region})
	}
	return buckets, nil
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

// result is the outcome of listing one bucket.
type result struct {
	bucket  Bucket
	size    float64
	objects float64
	err     error
}

// listAll lists every bucket, at most Concurrency of them at a time. The
// documented rate limit is per bucket and paging within one bucket is
// sequential by nature, so buckets are the only axis worth parallelising.
func (c *Collector) listAll(ctx context.Context, buckets []Bucket) []result {
	results := make([]result, len(buckets))
	sem := make(chan struct{}, c.concurrency)

	var wg sync.WaitGroup
	for i, b := range buckets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			size, objects, err := c.list(ctx, b)
			results[i] = result{bucket: b, size: size, objects: objects, err: err}
		}()
	}
	wg.Wait()
	return results
}

// list pages through one bucket, summing the sizes as it goes.
func (c *Collector) list(ctx context.Context, b Bucket) (size, objects float64, err error) {
	client := c.factory.Client(b.Region)
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(b.Name)})

	for pages.HasMorePages() {
		page, pageErr := pages.NextPage(ctx)
		if pageErr != nil {
			return 0, 0, fmt.Errorf("list objects of %q: %w", b.Name, pageErr)
		}
		for _, object := range page.Contents {
			objects++
			size += float64(aws.ToInt64(object.Size))
		}
	}
	return size, objects, nil
}

// merge builds the next snapshot. A bucket that failed carries its previous
// figures forward and is marked down, so one unreadable bucket never blanks
// the ones that were read.
func (c *Collector) merge(results []result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := make(map[string]stats, len(results))
	for _, r := range results {
		if r.err == nil {
			next[r.bucket.Name] = stats{
				region: r.bucket.Region, size: r.size, objects: r.objects, up: true, known: true,
			}
			continue
		}
		previous := c.snap[r.bucket.Name]
		previous.region = r.bucket.Region
		previous.up = false
		next[r.bucket.Name] = previous
	}
	c.snap = next
}

// Collect implements collector.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	for name, s := range snap {
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, boolToFloat(s.up), name, s.region)
		if !s.known {
			continue
		}
		ch <- prometheus.MustNewConstMetric(sizeDesc, prometheus.GaugeValue, s.size, name, s.region)
		ch <- prometheus.MustNewConstMetric(objectsDesc, prometheus.GaugeValue, s.objects, name, s.region)
	}
}

// boolToFloat maps a boolean to the 1/0 convention Prometheus expects.
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
