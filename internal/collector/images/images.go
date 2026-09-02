// Package images collects the private images of the account: droplet
// snapshots, volume snapshots, automatic droplet backups and uploaded custom
// images.
//
// Stored images are the classic forgotten DigitalOcean cost. Nothing in the
// control panel nags about a snapshot taken two years ago for a droplet that
// no longer exists, and DigitalOcean bills every gigabyte of it every month.
// One list request covers them all, which is what makes this cheap enough to
// leave on.
//
// Only the account's own images are read — the images endpoint with
// private=true. The public distribution and application images are
// DigitalOcean's, cost nothing and number in the hundreds.
package images

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kozaktomas/digitalocean_exporter/internal/paging"
)

// The API reports image sizes in gigabytes, read here as binary, the way the
// rest of this exporter reads DigitalOcean's gigabytes.
const bytesPerGibibyte = 1024 * 1024 * 1024

// knownTypes are the image types a private image can have. They are counted
// even when the account holds none of one, so that a snapshot policy that
// stopped running shows as a count falling to zero rather than as a series
// that quietly disappears.
var knownTypes = []string{"snapshot", "backup", "custom"}

// Metric descriptors. id, name and type identify an image across all of them;
// the labels that merely describe it live on the info metric, so that a
// distribution or a status changing does not churn the size series.
var (
	sizeDesc = prometheus.NewDesc("digitalocean_image_size_bytes",
		"Size of the stored image in bytes.", []string{"id", "name", "type"}, nil)
	minDiskDesc = prometheus.NewDesc("digitalocean_image_min_disk_size_bytes",
		"Smallest disk in bytes a droplet must have to boot this image.",
		[]string{"id", "name", "type"}, nil)
	createdDesc = prometheus.NewDesc("digitalocean_image_created_timestamp_seconds",
		"When the image was created, as a Unix timestamp.", []string{"id", "name", "type"}, nil)
	infoDesc = prometheus.NewDesc("digitalocean_image_info",
		"Always 1. Its labels describe the image.",
		[]string{"id", "name", "type", "distribution", "status", "regions"}, nil)
	countDesc = prometheus.NewDesc("digitalocean_images",
		"Number of private images of this type on the account.", []string{"type"}, nil)
)

// descriptors lists every metric the collector can emit.
var descriptors = []*prometheus.Desc{sizeDesc, minDiskDesc, createdDesc, infoDesc, countDesc}

// image is what one refresh learned about a single stored image.
type image struct {
	id           string
	name         string
	kind         string
	distribution string
	status       string
	regions      string

	size    float64
	minDisk float64

	// created is false for an image whose creation time the API did not
	// report or wrote in a format this cannot read. The timestamp is then
	// omitted rather than reported as the epoch, which would read as an image
	// created in 1970 and age past every threshold.
	created   bool
	createdAt float64
}

// snapshot is one whole refresh: the images and the per-type counts, replaced
// together so that a count can never disagree with the images behind it.
type snapshot struct {
	images []image
	counts map[string]float64
}

// Collector reports the private images of the account.
type Collector struct {
	client *godo.Client
	logger *slog.Logger

	mu   sync.RWMutex
	snap *snapshot
}

// New returns an images collector backed by client. The logger records what
// the scheduler never sees: a duplicate image dropped from a list that shifted
// between two page requests. A nil logger discards it.
func New(client *godo.Client, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Collector{client: client, logger: logger}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "images" }

// Describe implements collector.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

// Refresh implements collector.Collector. Every page is read before the
// snapshot is replaced, so a failure halfway through the list leaves the
// previous images in place rather than reporting half an account — which,
// for a metric people alert on the total of, would look exactly like somebody
// deleting snapshots.
func (c *Collector) Refresh(ctx context.Context) error {
	listed, err := paging.All(ctx, c.logger, "images",
		func(i godo.Image) int { return i.ID }, c.client.Images.ListUser)
	if err != nil {
		return err
	}

	next := &snapshot{
		images: make([]image, 0, len(listed)),
		counts: make(map[string]float64, len(knownTypes)),
	}
	for _, kind := range knownTypes {
		next.counts[kind] = 0
	}
	for i := range listed {
		img := newImage(&listed[i])
		next.images = append(next.images, img)
		next.counts[img.kind]++
	}

	c.mu.Lock()
	c.snap = next
	c.mu.Unlock()
	return nil
}

// newImage converts one API image into its snapshot form.
func newImage(i *godo.Image) image {
	out := image{
		id:           strconv.Itoa(i.ID),
		name:         i.Name,
		kind:         i.Type,
		distribution: i.Distribution,
		status:       i.Status,
		regions:      joinRegions(i.Regions),
		size:         i.SizeGigaBytes * bytesPerGibibyte,
		minDisk:      float64(i.MinDiskSize) * bytesPerGibibyte,
	}
	if created, err := time.Parse(time.RFC3339, i.Created); err == nil {
		out.created = true
		out.createdAt = float64(created.Unix())
	}
	return out
}

// joinRegions renders the regions an image is available in as one label. They
// are sorted first: the API's order is not documented as stable, and a label
// that reorders itself between two refreshes churns the series for no reason.
func joinRegions(regions []string) string {
	sorted := make([]string, len(regions))
	copy(sorted, regions)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// Collect implements collector.Collector. Before the first successful refresh
// it emits nothing; afterwards it emits the per-type counts even on an account
// holding no images at all.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()

	if snap == nil {
		return
	}

	for _, i := range snap.images {
		labels := []string{i.id, i.name, i.kind}
		gauge(ch, sizeDesc, i.size, labels...)
		gauge(ch, minDiskDesc, i.minDisk, labels...)
		if i.created {
			gauge(ch, createdDesc, i.createdAt, labels...)
		}
		gauge(ch, infoDesc, 1, i.id, i.name, i.kind, i.distribution, i.status, i.regions)
	}

	for kind, count := range snap.counts {
		gauge(ch, countDesc, count, kind)
	}
}

// gauge sends one gauge sample of desc with the given label values.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}
